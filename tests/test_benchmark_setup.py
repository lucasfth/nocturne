"""Setup gate tests never execute inference or service mutations."""
import json
from pathlib import Path

import pytest

from worker import benchmark_setup as setup
from worker import model_benchmark as m


def test_skip_private(tmp_path, monkeypatch):
    monkeypatch.setenv('NOCTURNE_SKIP_BENCHMARK', '1')
    monkeypatch.delenv('NOCTURNE_BENCHMARK_CATALOG', raising=False)
    p = tmp_path / 'result.json'
    assert setup.main(['--output', str(p)]) == 0
    assert json.loads(p.read_text())['status'] == 'unbenchmarked'
    assert p.stat().st_mode & 0o777 == 0o600
    assert setup.main(['--output', str(p)]) == 1


def test_required(tmp_path, monkeypatch):
    monkeypatch.delenv('NOCTURNE_SKIP_BENCHMARK', raising=False)
    monkeypatch.delenv('NOCTURNE_BENCHMARK_CATALOG', raising=False)
    p = tmp_path / 'result.json'
    assert setup.main(['--output', str(p)]) == 1
    assert json.loads(p.read_text())['reason'] == 'catalog_required'


@pytest.mark.parametrize('status,selected', [('no_suitable_candidate', None),
                                           ('refused_or_failed', None),
                                           ('completed', 'candidate_1')])
def test_recommendation_never_silently_applied(tmp_path, monkeypatch, status, selected):
    monkeypatch.delenv('NOCTURNE_SKIP_BENCHMARK', raising=False)
    monkeypatch.setenv('NOCTURNE_BENCHMARK_CATALOG', 'unused')
    c = {'server': setup.INSTALLER_SERVER, 'allowed_quantizations': ['Q4_K_M'],
         'profiles': [setup.INSTALLER_PROFILE], 'candidates': [dict(
        id='candidate_1', path=setup.INSTALLER_MODEL, parameters=9_000_000_000,
        quantization='Q4_K_M')]}
    monkeypatch.setattr(m, 'catalog', lambda _: c)
    monkeypatch.setattr(m, 'LOCK_ROOT', tmp_path)
    async def execute(c, report):
        report.update(status=status, selected=selected)
    monkeypatch.setattr(m, 'execute', execute)
    p = tmp_path / 'result.json'
    assert setup.main(['--output', str(p)]) == 1
    report = json.loads(p.read_text())
    if selected:
        assert report['reason'] == 'manual_apply_required'
    else:
        assert report['setup_gate'] == 'recommendation_failed'


def installer_catalog():
    return dict(server=setup.INSTALLER_SERVER, allowed_quantizations=['Q4_K_M'],
                profiles=[dict(setup.INSTALLER_PROFILE)], runtime_s=30,
                min_headroom_pct=20, disk_floor_gb=5, max_swap_growth_mb=0,
                candidates=[dict(id='candidate_1', path=setup.INSTALLER_MODEL,
                                 parameters=9_000_000_000, quantization='Q4_K_M')])


def test_compatible_and_exact_settings():
    c = installer_catalog()
    report = m.plan(c)
    report.update(status='completed', selected='candidate_1', cleanup=dict(
        owned_processes_stopped=True, owned_temp_removed=True, final_safety_verified=True))
    assert setup.compatible(c, report)
    c['profiles'][0]['spec_n'] = 1
    assert not setup.compatible(c, report)
    c['profiles'][0]['spec_n'] = 2
    c['profiles'].append(dict(setup.INSTALLER_PROFILE))
    assert not setup.compatible(c, report)


def test_unsupported_before_inference(tmp_path, monkeypatch):
    c = installer_catalog()
    c['profiles'][0]['spec_n'] = 0
    monkeypatch.setattr(m, 'catalog', lambda _: c)
    monkeypatch.setenv('NOCTURNE_BENCHMARK_CATALOG', 'unused')
    monkeypatch.delenv('NOCTURNE_SKIP_BENCHMARK', raising=False)
    async def forbidden(*_):
        pytest.fail('unsupported settings must not infer')
    monkeypatch.setattr(m, 'execute', forbidden)
    p = tmp_path / 'report.json'
    assert setup.main(['--output', str(p)]) == 1
    assert json.loads(p.read_text())['reason'] == 'installer_settings_mismatch'


def test_setup_real_sigterm_artifact(tmp_path, monkeypatch):
    import asyncio
    import os
    import signal
    from tests.test_model_benchmark import probes, FakeBackend
    c = installer_catalog()
    model = tmp_path / 'model.gguf'
    model.write_bytes(b'GGUFfake')
    monkeypatch.setattr(setup, 'INSTALLER_MODEL', model)
    c['candidates'][0]['path'] = model
    monkeypatch.setattr(m, 'catalog', lambda _: c)
    monkeypatch.setattr(m, 'LOCK_ROOT', tmp_path)
    monkeypatch.setenv('NOCTURNE_BENCHMARK_CATALOG', 'unused')
    monkeypatch.delenv('NOCTURNE_SKIP_BENCHMARK', raising=False)
    execute = m.execute
    class Hanging(FakeBackend):
        async def start(self):
            asyncio.get_running_loop().call_later(.02, os.kill, os.getpid(), signal.SIGTERM)
            await asyncio.sleep(60)
    async def fake_version(_):
        return 'test'
    monkeypatch.setattr(m, 'backend_version', fake_version)
    async def wrapped(c, report):
        await execute(c, report, probes, Hanging)
    monkeypatch.setattr(m, 'execute', wrapped)
    p = tmp_path / 'interrupted.json'
    assert setup.main(['--output', str(p)]) == 1
    report = json.loads(p.read_text())
    assert report['status'] == 'interrupted'
    assert report['cleanup']['owned_processes_stopped']
    assert report['cleanup']['owned_temp_removed']
    assert report['selected'] is None


def test_installer_gate_precedes_mutations():
    text = Path('worker/install-services.sh').read_text()
    gate = text.index('-m benchmark_setup')
    for mutation in ('$S mkdir', '$S install', '$S tee', 'launchctl bootout', 'launchctl bootstrap'):
        assert gate < text.index(mutation)
    assert 'worker_run /usr/bin/env PYTHONPATH="$PACKAGE"' in text
    assert '/tmp/nocturne-pkg' not in text
