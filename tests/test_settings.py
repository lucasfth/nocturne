import asyncio
from concurrent.futures import ThreadPoolExecutor
import json
import os

import httpx
import pytest
from fastapi import HTTPException
from gateway.settings import Settings
from test_gateway import c


def test_http_config(c):
    async def scenario():
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin), base_url='http://test') as client:
            for method in ['GET', 'PATCH']:
                assert (await client.request(method, '/admin/config', json={})).status_code == 401
            client.headers['Authorization'] = 'Bearer admin'
            r = await client.get('/admin/config')
            assert r.headers['cache-control'] == 'no-store'
            v = r.json()
            assert set(v['values']) == {'service_name', 'public_limit_concurrency'}
            assert 'admin' not in json.dumps(v['values'])
            patch = {'revision': v['revision'], 'changes': {'service_name': 'My inference', 'public_limit_concurrency': 20}}
            for headers in [{'Origin': 'https://evil.example'}, {'Sec-Fetch-Site': 'cross-site'}, {'Tailscale-User-Login': 'outsider'}]:
                assert (await client.patch('/admin/config', json=patch, headers=headers)).status_code == 403
            assert not c.settings.path.exists()
            before = (c.root / 'config.json').read_bytes()
            for changes in [{'public_limit_concurrency': True}, {'service_name': '<script>'}, {'lightning': {}}, {'service_name': None}, {'public_limit_concurrency': 0}]:
                assert (await client.patch('/admin/config', json={**patch, 'changes': changes})).status_code == 400
                assert not c.settings.path.exists()
                assert (c.root / 'config.json').read_bytes() == before
            result = (await client.patch('/admin/config', json=patch)).json()
            assert result['restart_required'] == ['public_limit_concurrency']
            assert result['effective']['service_name'] == 'My inference'
            assert result['effective']['public_limit_concurrency'] == 64
            assert (await client.patch('/admin/config', json=patch)).status_code == 409
            assert os.stat(c.settings.path).st_mode & 0o777 == 0o600
            assert os.stat(c.settings.path.parent).st_mode & 0o777 == 0o700
            assert (c.root / 'config.json').read_bytes() == before
            from gateway.app import Coordinator
            restarted = Coordinator(c.root)
            assert restarted.settings.view()['restart_required'] == []
            assert restarted.config['public_limit_concurrency'] == 20
            assert restarted.config['service_name'] == 'My inference'
    asyncio.run(scenario())


def test_redaction_preservation_and_concurrency(tmp_path):
    path = tmp_path / 'config.json'
    path.write_text(json.dumps({'signup_invite': 'private-value', 'lightning': {'password': 'hidden-value'}}))
    store = Settings(path, {})
    assert 'private-value' not in json.dumps(store.view())
    assert 'hidden-value' not in json.dumps(store.view())
    revision = store.revision
    def patch(n):
        try:
            store.patch({'revision': revision, 'changes': {'service_name': f'Name {n}'}})
            return 200
        except HTTPException as e:
            return e.status_code
    with ThreadPoolExecutor(max_workers=8) as pool:
        results = list(pool.map(patch, range(8)))
    assert results.count(200) == 1 and results.count(409) == 7
    assert json.loads(path.read_text())['lightning']['password'] == 'hidden-value'


def test_atomic_failure_and_directory_fsync(tmp_path, monkeypatch):
    path = tmp_path / 'config.json'
    path.write_text('{}')
    store = Settings(path, {})
    patch = {'revision': store.revision, 'changes': {'service_name': 'Changed'}}
    original = os.fsync
    calls = []
    def fsync(fd):
        calls.append(os.fstat(fd).st_mode)
        return original(fd)
    monkeypatch.setattr(os, 'fsync', fsync)
    store.patch(patch)
    import stat
    assert stat.S_ISREG(calls[0]) and stat.S_ISDIR(calls[1])
    before = store.path.read_bytes()
    def fail(fd):
        raise OSError('private disk details')
    monkeypatch.setattr(os, 'fsync', fail)
    with pytest.raises(HTTPException) as err:
        store.patch({**patch, 'revision': store.revision})
    assert err.value.status_code == 503 and store.path.read_bytes() == before
    assert path.read_bytes() == b'{}'
    assert not list(store.path.parent.glob('.nocturne-settings-*'))


def test_external_edit_not_overwritten(tmp_path):
    path = tmp_path / 'config.json'
    path.write_text('{}')
    store = Settings(path, {})
    path.write_text('{"extension": 1}')
    with pytest.raises(HTTPException) as err:
        store.patch({'revision': store.revision, 'changes': {'service_name': 'Changed'}})
    assert err.value.status_code == 409
    assert json.loads(path.read_text()) == {'extension': 1}


def test_directory_fsync_failure_blocks_store(tmp_path, monkeypatch):
    path = tmp_path / 'config.json'
    path.write_text('{}')
    runtime = {}
    store = Settings(path, runtime)
    original = os.fsync
    def fail_directory(fd):
        import stat
        if stat.S_ISDIR(os.fstat(fd).st_mode):
            raise OSError('directory unavailable')
        original(fd)
    monkeypatch.setattr(os, 'fsync', fail_directory)
    with pytest.raises(HTTPException) as err:
        store.patch({'revision': store.revision, 'changes': {'service_name': 'Saved'}})
    assert err.value.status_code == 503
    assert runtime == {}
    assert json.loads(store.path.read_text())['service_name'] == 'Saved'
    assert path.read_bytes() == b'{}'
    with pytest.raises(HTTPException) as err:
        store.view()
    assert err.value.status_code == 503
