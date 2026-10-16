"""Local inventory and POSIX protected-installation benchmark binding.

Inventory is deliberately one directory level, never follows symlinks, and does
not install anything. Execution requires root-controlled ancestors and immutable
(to non-root writers) files. Root and pre-existing writable descriptors are trusted.
"""
from __future__ import annotations

from contextlib import ExitStack
import hashlib
import os
from pathlib import Path
import stat
import sys

from . import benchmark_discovery as discovery
from .model_benchmark import Backend, Refused


MAX_ENTRIES = 1024
MAX_MODELS = 64
MAX_TOTAL_BYTES = 512 * 2**30


def inventory(directory, *, output_reserve=256, max_entries=MAX_ENTRIES,
              max_models=MAX_MODELS, max_total_bytes=MAX_TOTAL_BYTES):
    """Flat inventory owned by the current real UID (not an inferred operator).

    Production uses its separately validated root-owned installed inventory;
    custom inventory providers must explicitly enforce their own owner policy.
    """
    for value, ceiling in ((max_entries, MAX_ENTRIES), (max_models, MAX_MODELS),
                           (max_total_bytes, MAX_TOTAL_BYTES)):
        if type(value) is not int or not 1 <= value <= ceiling:
            raise Refused('inventory_limit')
    path = Path(os.path.abspath(directory))
    if '..' in Path(directory).parts:
        raise Refused('parent_path_unsupported')
    fd = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        if os.fstat(fd).st_uid != os.getuid():
            raise Refused('inventory_owner')
        paths, total = [], 0
        with os.scandir(fd) as entries:
            for count, entry in enumerate(entries, 1):
                if count > max_entries:
                    raise Refused('inventory_limit')
                if not entry.name.lower().endswith('.gguf'):
                    continue
                s = entry.stat(follow_symlinks=False)
                if not stat.S_ISREG(s.st_mode) or s.st_uid != os.getuid():
                    raise Refused('inventory_file_policy')
                total += s.st_size
                paths.append(path / entry.name)
                if len(paths) > max_models or total > max_total_bytes:
                    raise Refused('inventory_limit')
        models = discovery.discover(sorted(paths), output_reserve=output_reserve)
        # Discovery reopens paths without symlinks; ensure it inspected this directory.
        if discovery._identity(os.stat(path, follow_symlinks=False)) != discovery._identity(os.fstat(fd)):
            raise Refused('file_changed')
        return models
    except OSError:
        raise Refused('local_file_unavailable') from None
    finally:
        os.close(fd)


def no_acl(fd):
    """Reject any macOS extended ACL on the actual opened object, fail closed.

    Use the fixed system library, not a PATH command or daemon import. One native
    entry lookup bounds work/output regardless of ACL size; the descriptor avoids
    pathname races. POSIX mode policy remains mandatory on other platforms.
    """
    if sys.platform != 'darwin':
        return
    import ctypes
    import errno

    try:
        lib = ctypes.CDLL('/usr/lib/libSystem.B.dylib', use_errno=True)
        lib.acl_get_fd_np.argtypes = [ctypes.c_int, ctypes.c_int]
        lib.acl_get_fd_np.restype = ctypes.c_void_p
        lib.acl_get_entry.argtypes = [ctypes.c_void_p, ctypes.c_int,
                                     ctypes.POINTER(ctypes.c_void_p)]
        lib.acl_get_entry.restype = ctypes.c_int
        lib.acl_free.argtypes = [ctypes.c_void_p]
        lib.acl_free.restype = ctypes.c_int
        ctypes.set_errno(0)
        acl = lib.acl_get_fd_np(fd, 0x100)  # ACL_TYPE_EXTENDED
        if not acl:
            # Darwin reports an absent extended ACL as ENOENT, including for
            # existing files. No pathname lookup is involved with this API.
            if ctypes.get_errno() == errno.ENOENT:
                return
            raise Refused('acl_unavailable')
        try:
            entry = ctypes.c_void_p()
            # Darwin returns 0 for an entry, -1/EINVAL at end (not POSIX 1/0).
            ctypes.set_errno(0)
            result = lib.acl_get_entry(acl, 0, ctypes.byref(entry))  # ACL_FIRST_ENTRY
            if result != -1 or ctypes.get_errno() != errno.EINVAL:
                raise Refused('acl_not_allowed')
        finally:
            lib.acl_free(acl)
    except (OSError, AttributeError):
        raise Refused('acl_unavailable') from None


def _protected_open(path, *, executable=False):
    """Canonical no-symlink walk; every ancestor is root-controlled."""
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise Refused('protected_path')
    fd = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:]:
            s = os.fstat(fd)
            if not stat.S_ISDIR(s.st_mode) or s.st_uid != 0 or s.st_mode & 0o022:
                raise Refused('protected_ancestor')
            no_acl(fd)
            child = os.open(part, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                            dir_fd=fd)
            os.close(fd)
            fd = child
        s = os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or s.st_uid != 0 or s.st_mode & 0o222:
            raise Refused('protected_file')
        no_acl(fd)
        if executable and not s.st_mode & 0o111:
            raise Refused('build_not_executable')
        result, fd = fd, None
        return result
    except OSError:
        raise Refused('protected_path') from None
    finally:
        if fd is not None:
            os.close(fd)


def digest_fd(fd):
    """Streaming SHA256 without changing the shared descriptor offset."""
    metadata = os.fstat(fd)
    if not stat.S_ISREG(metadata.st_mode) or not 0 <= metadata.st_size <= MAX_TOTAL_BYTES:
        raise Refused('digest_limit')
    before = discovery._identity(metadata)
    digest, offset = hashlib.sha256(), 0
    while offset < before[2]:
        data = os.pread(fd, min(2**20, before[2] - offset), offset)
        if not data:
            raise Refused('file_changed')
        digest.update(data)
        offset += len(data)
    if discovery._identity(os.fstat(fd)) != before:
        raise Refused('file_changed')
    return digest.hexdigest()


def local_config(server, models):
    """In-memory runner config; strict discovery quality and 32768 context per slot."""
    models = list(models)
    if not 1 <= len(models) <= MAX_MODELS:
        raise Refused('candidate_count')
    if any(m.output_reserve != 256 or m.context_length < 32768 or
           not set(m.tensor_types) <= discovery.DEFAULT_QUALITY for m in models):
        raise Refused('model_policy')
    return dict(server=Path(server), min_context=32768,
                allowed_quantizations=sorted(discovery.DEFAULT_QUALITY),
                runtime_s=600, tokens=256, repeats=2, disk_floor_gb=10,
                min_prefill_tps=1, min_decode_tps=1, min_headroom_pct=20,
                max_swap_growth_mb=0,
                profiles=[dict(context=32768, parallel=2, spec_n=2)],
                candidates=[dict(id=f'candidate_{i+1}', path=m.path,
                                 parameters=m.parameters, quantization=m.quantization)
                            for i, m in enumerate(models)])


class InstalledArtifacts:
    """Keep open through execute(..., backend_factory=installed.backend_factory).

    The build runs by protected canonical pathname, NOT by descriptor (script
    descriptor execution is not portable). Model bytes are never copied.
    """
    def __init__(self, server, models):
        self.server, self.models = Path(server), tuple(models)
        self._stack = None
        self._files = {}

    def __enter__(self):
        if self._stack is not None:
            raise Refused('already_open')
        self._stack = ExitStack()
        try:
            for path, executable in [(self.server, True)] + [(m.path, False) for m in self.models]:
                if path in self._files:
                    raise Refused('duplicate_file')
                fd = _protected_open(path, executable=executable)
                self._stack.callback(os.close, fd)
                self._files[path] = (fd, discovery._identity(os.fstat(fd)))
            for model in self.models:
                fd, identity = self._files[model.path]
                if identity != model.identity or digest_fd(fd) != model.sha256:
                    raise Refused('file_changed')
                # Do not trust caller-constructed Model metadata or relaxed discovery.
                if discovery.inspect_gguf(model.path, output_reserve=256) != model:
                    raise Refused('model_policy')
            self.config = local_config(self.server, self.models)
            self.verify()
            return self
        except BaseException:
            self._stack.close()
            self._stack = None
            self._files.clear()
            raise

    def verify(self):
        if self._stack is None:
            raise Refused('artifacts_closed')
        for path, (fd, identity) in self._files.items():
            if discovery._identity(os.fstat(fd)) != identity:
                raise Refused('file_changed')
            current = _protected_open(path, executable=path == self.server)
            try:
                if discovery._identity(os.fstat(current)) != identity:
                    raise Refused('file_changed')
            finally:
                os.close(current)

    def backend_factory(self, c, model, profile, directory):
        installed = self
        if c['server'] != self.server or model not in self.config['candidates']:
            raise Refused('artifact_mismatch')

        class VerifiedBackend(Backend):
            def artifact_binding(self):
                installed.verify()
                fd = installed._files[Path(self.model['path'])][0]
                return f'/dev/fd/{fd}', {'pass_fds': (fd,)}

            async def start(self):
                await super().start()
                installed.verify()

            async def stop(self):
                clean = await super().stop()
                installed.verify()
                return clean

        return VerifiedBackend(c, model, profile, directory)

    def __exit__(self, *_):
        try:
            self.verify()
        finally:
            self._stack.close()
            self._stack = None
            self._files.clear()
