"""Offline synthetic artifacts; protected-install policy mocked only for spawn tests."""
import asyncio
import hashlib
import os
from pathlib import Path
import ctypes
import errno
import subprocess
import sys

import pytest

from worker import benchmark_artifacts as a
from worker import benchmark_discovery as d
from test_benchmark_discovery import gguf


def model_file(tmp_path):
    path = tmp_path.resolve() / 'model.gguf'
    path.write_bytes(gguf())
    return path


def test_inventory_policy_and_caps(tmp_path):
    path = model_file(tmp_path)
    models = a.inventory(path.parent)
    assert models[0].sha256 == hashlib.sha256(path.read_bytes()).hexdigest()
    c = a.local_config('/protected/server', models)
    assert c['profiles'] == [dict(context=32768, parallel=2, spec_n=2)]
    assert c['tokens'] == 256
    with pytest.raises(a.Refused, match='inventory_limit'):
        a.inventory(path.parent, max_total_bytes=1)
    (path.parent / 'other.gguf').write_bytes(gguf())
    with pytest.raises(a.Refused, match='inventory_limit'):
        a.inventory(path.parent, max_models=1)
    with pytest.raises(a.Refused, match='inventory_limit'):
        a.inventory(path.parent, max_entries=1)


def test_symlinks_and_quality(tmp_path):
    path = model_file(tmp_path)
    (path.parent / 'link.gguf').symlink_to(path)
    with pytest.raises(a.Refused, match='inventory_file_policy'):
        a.inventory(path.parent)
    (path.parent / 'link.gguf').unlink()
    path.write_bytes(gguf(context=8192))
    with pytest.raises(d.Refused, match='context_length'):
        a.inventory(path.parent)
    path.write_bytes(gguf(tensors=((12, (256, 2)),)))
    with pytest.raises(d.Refused, match='quantization_policy'):
        a.inventory(path.parent)


def test_unprotected_install_refused(tmp_path):
    path = model_file(tmp_path)
    with pytest.raises(a.Refused, match='protected_'):
        a._protected_open(path)


def fixture_install(tmp_path, monkeypatch):
    path = model_file(tmp_path)
    models = a.inventory(path.parent)
    server = path.parent / 'server'
    # A harmless executable using the REAL Backend launch and inherited fd.
    server.write_text('#!/bin/sh\n/bin/cat "$2" > "$HOME/readback"\n')
    server.chmod(0o500)
    monkeypatch.setattr(a, '_protected_open',
                        lambda path, **kw: os.open(path, os.O_RDONLY | os.O_NOFOLLOW))
    return path, a.InstalledArtifacts(server, models)


def test_actual_backend_descriptor_survives_replacement(tmp_path, monkeypatch):
    path, installed = fixture_install(tmp_path, monkeypatch)
    original = path.read_bytes()
    real_spawn = asyncio.create_subprocess_exec

    async def attacked_spawn(*args, **kwargs):
        assert args[2].startswith('/dev/fd/')
        assert kwargs['pass_fds']
        path.rename(path.with_suffix('.old'))
        path.write_bytes(b'SUBSTITUTED')
        return await real_spawn(*args, **kwargs)

    monkeypatch.setattr(asyncio, 'create_subprocess_exec', attacked_spawn)

    async def run():
        b = installed.backend_factory(installed.config, installed.config['candidates'][0],
                                      installed.config['profiles'][0], str(path.parent))
        async def ready(*args):
            await b.process.wait()
        b.request = ready
        try:
            with pytest.raises(a.Refused, match='file_changed'):
                await b.start()
        finally:
            with pytest.raises(a.Refused, match='file_changed'):
                await b.stop()

    with pytest.raises(a.Refused, match='file_changed'):
        with installed:
            asyncio.run(run())
    assert (path.parent / 'readback').read_bytes() == original
    assert installed._stack is None


def test_mutation_and_descriptor_lifetime(tmp_path, monkeypatch):
    path, installed = fixture_install(tmp_path, monkeypatch)
    with pytest.raises(a.Refused, match='file_changed'):
        with installed:
            fd = installed._files[path][0]
            assert a.digest_fd(fd) == hashlib.sha256(path.read_bytes()).hexdigest()
            with path.open('r+b') as stream:
                stream.seek(-1, 2)
                stream.write(b'X')
            with pytest.raises(a.Refused, match='file_changed'):
                installed.verify()
    with pytest.raises(OSError):
        os.fstat(fd)


def test_changed_before_enter(tmp_path, monkeypatch):
    path, installed = fixture_install(tmp_path, monkeypatch)
    path.write_bytes(gguf() + b'changed')
    with pytest.raises(a.Refused, match='file_changed'):
        with installed:
            pass
    assert installed._stack is None


def mock_root_modes(monkeypatch):
    real = os.fstat

    def trusted(fd):
        fields = list(real(fd))
        fields[0] &= ~0o222
        fields[4] = 0
        return os.stat_result(fields)

    monkeypatch.setattr(os, 'fstat', trusted)


def test_acl_checked_at_every_component(tmp_path, monkeypatch):
    path = model_file(tmp_path)
    mock_root_modes(monkeypatch)
    seen = []
    monkeypatch.setattr(a, 'no_acl', lambda fd: seen.append(os.fstat(fd).st_ino))
    fd = a._protected_open(path)
    os.close(fd)
    components = [*reversed(path.parents), path]
    assert seen == [p.stat().st_ino for p in components]
    for target in seen:
        def deny(fd):
            if os.fstat(fd).st_ino == target:
                raise a.Refused('acl_not_allowed')
        monkeypatch.setattr(a, 'no_acl', deny)
        with pytest.raises(a.Refused, match='acl_not_allowed'):
            a._protected_open(path)


def test_symlink_ancestor_refused_even_with_trusted_modes(tmp_path, monkeypatch):
    path = model_file(tmp_path)
    link = path.parent / 'linked-directory'
    link.symlink_to(path.parent, target_is_directory=True)
    mock_root_modes(monkeypatch)
    monkeypatch.setattr(a, 'no_acl', lambda fd: None)
    with pytest.raises(a.Refused, match='protected_path'):
        a._protected_open(link / path.name)


@pytest.mark.parametrize('pointer,error,result,entry_error,refusal', [
    (None, errno.ENOENT, -1, errno.EINVAL, None),
    (None, errno.EACCES, -1, errno.EINVAL, 'acl_unavailable'),
    (1, 0, 0, 0, 'acl_not_allowed'),
    (1, 0, -1, errno.EINVAL, None),
    (1, 0, -1, errno.EIO, 'acl_not_allowed'),
])
def test_darwin_acl_api(monkeypatch, pointer, error, result, entry_error, refusal):
    from types import SimpleNamespace
    freed = []

    def get(fd, kind):
        assert (fd, kind) == (42, 0x100)
        ctypes.set_errno(error)
        return pointer

    def entry(acl, which, output):
        assert (acl, which) == (1, 0)
        ctypes.set_errno(entry_error)
        return result

    def free(acl):
        freed.append(acl)
        return 0

    lib = SimpleNamespace(acl_get_fd_np=get, acl_get_entry=entry, acl_free=free)
    monkeypatch.setattr(a.sys, 'platform', 'darwin')
    monkeypatch.setattr(ctypes, 'CDLL', lambda path, **kw: lib)
    if refusal:
        with pytest.raises(a.Refused, match=refusal):
            a.no_acl(42)
    else:
        a.no_acl(42)
    assert freed == ([1] if pointer else [])


@pytest.mark.skipif(sys.platform != 'darwin', reason='native macOS ACL')
@pytest.mark.parametrize('directory', [False, True])
def test_native_named_acl(tmp_path, directory):
    path = tmp_path / 'ancestor' if directory else model_file(tmp_path)
    if directory:
        path.mkdir()
    path.chmod(0o555 if directory else 0o444)
    fd = os.open(path, os.O_RDONLY)
    try:
        a.no_acl(fd)
        subprocess.run(['/bin/chmod', '+a', 'everyone allow write', str(path)],
                       check=True, timeout=5, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL)
        with pytest.raises(a.Refused, match='acl_not_allowed'):
            a.no_acl(fd)
    finally:
        os.close(fd)
        path.chmod(0o700 if directory else 0o600)


def test_build_digest_streaming_bounded_and_offset_preserved(tmp_path, monkeypatch):
    path = tmp_path / 'build'
    data = b'x' * (2**20 + 17)
    path.write_bytes(data)
    fd = os.open(path, os.O_RDONLY)
    pread = os.pread
    reads = []

    def bounded(fd, size, offset):
        assert size <= 2**20
        reads.append(size)
        return pread(fd, size, offset)

    monkeypatch.setattr(os, 'pread', bounded)
    try:
        os.lseek(fd, 7, os.SEEK_SET)
        assert a.digest_fd(fd) == hashlib.sha256(data).hexdigest()
        assert reads == [2**20, 17]
        assert os.lseek(fd, 0, os.SEEK_CUR) == 7
        monkeypatch.setattr(a, 'MAX_TOTAL_BYTES', 1)
        with pytest.raises(a.Refused, match='digest_limit'):
            a.digest_fd(fd)
    finally:
        os.close(fd)


def test_build_identity_substitution_refused_before_spawn(tmp_path, monkeypatch):
    path, installed = fixture_install(tmp_path, monkeypatch)
    with pytest.raises(a.Refused, match='file_changed'):
        with installed:
            server = installed.server
            server.rename(server.with_suffix('.old'))
            server.write_text('#!/bin/sh\nexit 0\n')
            backend = installed.backend_factory(installed.config,
                        installed.config['candidates'][0],
                        installed.config['profiles'][0], str(path.parent))
            with pytest.raises(a.Refused, match='file_changed'):
                backend.artifact_binding()
            assert backend.process is None
