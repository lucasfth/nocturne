"""Persistence gate: only private typed settings state is writable."""
import json
import os
import stat

import pytest
from fastapi import HTTPException

from gateway.settings import Settings, load_config, overlay_path


@pytest.fixture
def base(tmp_path):
    path = tmp_path / 'config.json'
    path.write_text(json.dumps({
        'service_name': 'Base name', 'public_limit_concurrency': 73,
        'lightning': {'backend': 'fake'}, 'signup_invite': 'fixture-only',
        'admin_tailnet_logins': ['fixture'],
        'extension': {'number': 9007199254740993, 'password': 'fixture-only'},
    }))
    (tmp_path / 'data').mkdir(mode=0o700)
    return path


def patch(store, **changes):
    return store.patch({'revision': store.revision, 'changes': changes})


def test_readonly_base_restart_and_sparse_overlay(base):
    before = base.read_bytes()
    base.chmod(0o400)
    base.parent.chmod(0o500)
    try:
        runtime = load_config(base)
        store = Settings(base, runtime)
        assert not store.path.exists()
        assert store.path == base.parent / 'data/settings/config.json'
        assert store.view()['values']['public_limit_concurrency'] == 73
        patch(store, service_name='Portable 😀 \\unicode')
        assert json.loads(store.path.read_bytes()) == {'service_name': 'Portable 😀 \\unicode'}
        patch(store, public_limit_concurrency=20)
        assert store.view()['restart_required'] == ['public_limit_concurrency']
        old_revision = store.revision
        restarted = Settings(base, load_config(base))
        assert restarted.view()['values'] == restarted.view()['effective']
        assert restarted.view()['restart_required'] == []
        assert restarted.runtime['lightning'] == {'backend': 'fake'}
        assert restarted.runtime['extension']['number'] == 9007199254740993
        with pytest.raises(HTTPException) as err:
            restarted.patch({'revision': old_revision, 'changes': {'service_name': 'Stale'}})
        assert err.value.status_code == 409
        assert base.read_bytes() == before
        assert stat.S_IMODE(base.stat().st_mode) == 0o400
        assert stat.S_IMODE(base.parent.stat().st_mode) == 0o500
        assert stat.S_IMODE(store.path.stat().st_mode) == 0o600
        assert stat.S_IMODE(store.path.parent.stat().st_mode) == 0o700
        assert set(json.loads(store.path.read_bytes())) == {'service_name', 'public_limit_concurrency'}
        assert set(p.name for p in base.parent.iterdir()) == {'data', 'config.json'}
    finally:
        base.parent.chmod(0o700)
        base.chmod(0o600)


@pytest.mark.parametrize('raw, expected', [
    (b'{}', {'service_name': 'Base name', 'public_limit_concurrency': 73}),
    ('{"service_name":"Portable 😀 \\\\unicode","public_limit_concurrency":20}'.encode(),
     {'service_name': 'Portable 😀 \\unicode', 'public_limit_concurrency': 20}),
    (b'{"public_limit_concurrency":1}', {'service_name': 'Base name', 'public_limit_concurrency': 1}),
    (b'{"public_limit_concurrency":4096}', {'service_name': 'Base name', 'public_limit_concurrency': 4096}),
])
def test_go_compatible_overlay_load(base, raw, expected):
    path = overlay_path(base)
    path.parent.mkdir(mode=0o700)
    path.write_bytes(raw)
    path.chmod(0o600)
    store = Settings(base, load_config(base))
    assert store.view()['values'] == expected
    assert store.view()['effective'] == expected
    assert store.view()['restart_required'] == []


@pytest.mark.parametrize('edit', ['create', 'replace', 'delete'])
def test_overlay_external_cas(base, edit):
    store = Settings(base, load_config(base))
    if edit != 'create':
        patch(store, service_name='Saved')
    if edit == 'delete':
        store.path.unlink()
        external = None
    else:
        external = b'{"service_name":"External"}'
        store.path.write_bytes(external)
        store.path.chmod(0o600)
    with pytest.raises(HTTPException) as err:
        patch(store, service_name='Lost update')
    assert err.value.status_code == 409
    assert (store.path.read_bytes() if store.path.exists() else None) == external


INVALID_OVERLAYS = [
    b'null', b'[]', b'{', b'{} {}', b' ' * 4097, b'[' * 1500 + b']' * 1500,
    b'{"service_name":"A","service_name":"B"}',
    b'{"service_name":null}', b'{"service_name":true}', b'{"service_name":{}}',
    b'{"service_name":""}', b'{"service_name":"<script>"}',
    b'{"service_name":"\\ud800"}', b'{"service_name":"\\u00a0"}',
    b'{"service_name":"\xff"}',
    b'{"public_limit_concurrency":true}', b'{"public_limit_concurrency":1.0}',
    b'{"public_limit_concurrency":1e2}', b'{"public_limit_concurrency":null}',
    b'{"public_limit_concurrency":0}', b'{"public_limit_concurrency":4097}',
    b'{"public_limit_concurrency":NaN}',
    b'{"lightning":{"backend":"fake"}}', b'{"admin_tailnet_logins":[]}',
    b'{"extension":{}}', b'{"service_name":"Good","signup_invite":"fixture-only"}',
]


@pytest.mark.parametrize('raw', INVALID_OVERLAYS, ids=range(len(INVALID_OVERLAYS)))
def test_startup_rejects_invalid_overlay(base, raw):
    path = overlay_path(base)
    path.parent.mkdir(mode=0o700)
    path.write_bytes(raw)
    path.chmod(0o600)
    before = base.read_bytes()
    for action in (lambda: load_config(base), lambda: Settings(base, {})):
        with pytest.raises(ValueError, match='^configuration settings startup failed$'):
            action()
    assert base.read_bytes() == before
    assert path.read_bytes() == raw


@pytest.mark.parametrize('kind', ['directory-mode', 'file-mode', 'directory-link', 'file-link', 'data-link', 'not-file'])
def test_startup_rejects_unsafe_state(base, kind):
    path = overlay_path(base)
    path.parent.mkdir(mode=0o700)
    path.write_bytes(b'{}')
    path.chmod(0o600)
    if kind == 'directory-mode':
        path.parent.chmod(0o755)
    elif kind == 'file-mode':
        path.chmod(0o644)
    elif kind == 'directory-link':
        moved = path.parent.with_name('other')
        path.parent.rename(moved)
        path.parent.symlink_to(moved, target_is_directory=True)
    elif kind == 'data-link':
        data = path.parent.parent
        moved = data.with_name('other')
        data.rename(moved)
        data.symlink_to(moved, target_is_directory=True)
    elif kind == 'file-link':
        path.unlink()
        path.symlink_to(base)
    else:
        path.unlink()
        path.mkdir(mode=0o700)
    for action in (lambda: load_config(base), lambda: Settings(base, {})):
        with pytest.raises(ValueError, match='^configuration settings startup failed$'):
            action()


def test_new_directory_is_durable(base, monkeypatch):
    calls = []
    original = os.fsync
    def sync(fd):
        calls.append(os.fstat(fd).st_ino)
        original(fd)
    monkeypatch.setattr(os, 'fsync', sync)
    store = Settings(base, load_config(base))
    assert calls == [(base.parent / 'data').stat().st_ino]
    patch(store, service_name='Saved')
    assert calls[1:] == [store.path.stat().st_ino, store.path.parent.stat().st_ino]


@pytest.mark.parametrize('stage', ['file-sync', 'rename'])
def test_first_write_failure_is_retryable(base, monkeypatch, stage):
    runtime = load_config(base)
    store = Settings(base, runtime)
    old_revision = store.revision
    def fail(*args):
        raise OSError('fixture-only disk error')
    with monkeypatch.context() as m:
        m.setattr(os, 'fsync' if stage == 'file-sync' else 'replace', fail)
        with pytest.raises(HTTPException) as err:
            patch(store, service_name='Changed')
        assert err.value.status_code == 503
        assert 'fixture-only' not in str(err.value)
    assert store.revision == old_revision
    assert runtime['service_name'] == 'Base name'
    assert not store.path.exists()
    assert list(store.path.parent.iterdir()) == []
    assert patch(store, service_name='Retry')['effective']['service_name'] == 'Retry'


def test_legacy_base_name_remains_compatible(tmp_path):
    from gateway.settings import Settings, load_config
    import json
    path = tmp_path / 'config.json'
    path.write_text(json.dumps({'service_name': 'Research & Development'}))
    runtime = load_config(path)
    settings = Settings(path, runtime)
    assert runtime['service_name'] == 'Research & Development'
    settings.patch({'revision': settings.view()['revision'], 'changes': {'service_name': 'Updated'}})
    assert load_config(path)['service_name'] == 'Updated'
    assert json.loads(path.read_text())['service_name'] == 'Research & Development'
