"""Installed exceptions are identity-bound; all live safety readings still apply."""
import os
import asyncio
import functools
from types import SimpleNamespace

import pytest

from worker import model_benchmark as b
from worker.benchmark_production import InstalledProbe

def async_test(fn):
    @functools.wraps(fn)
    def run(*args, **kwargs):
        return asyncio.run(fn(*args, **kwargs))
    return run


START = 'Mon Jan 1 12:00:00 2026'
EXE = '/opt/nocturne/python'


@pytest.fixture
def host(monkeypatch):
    state = dict(system='io.nocturne.worker\nio.nocturne.tunnel\n', optional='',
                 disabled='"io.nocturne.llama" => true', extra='', mismatch=False,
                 pressure='1', admissions=0, start=START, runner_start=START)
    runner = os.getpid()

    def call(op):
        assert op == 'admission'
        state['admissions'] += 1
        return dict(worker_pid=42, worker_executable=EXE, worker_start=START)

    async def command(*args, **kw):
        if args[0].endswith('sysctl'):
            return {'machdep.cpu.brand_string': 'Apple M4', 'hw.memsize': str(32 * 2**30),
                    'kern.memorystatus_level': '80',
                    'kern.memorystatus_vm_pressure_level': state['pressure'],
                    'vm.swapusage': 'used = 0M'}[args[-1]]
        if args[0].endswith('pmset'):
            return "'AC Power' 100%"
        if args[0].endswith('ioreg'):
            return '"HIDIdleTime" = 300000000000'
        if args[0].endswith('launchctl'):
            if args[1] == 'print-disabled':
                return state['disabled']
            return state['system'] if args[-1] == 'system' else state['optional']
        if args[0].endswith('/ps'):
            if '-p' in args:
                return f'{runner} {state["runner_start"]} {EXE}\n'
            if 'lstart' in args[-1]:
                exe = '/other/python' if state['mismatch'] else EXE
                return f'42 {state["start"]} {exe}\n{runner} {START} {EXE}\n99 {START} /usr/bin/python3\n'
            if args[-1] == 'pid=,comm=':
                return f'42 {EXE}\n{runner} {EXE}\n99 /usr/bin/python3\n'
            return f'42 10 python -m worker.agent\n{runner} 10 python runner\n' + state['extra']
        if args[0].endswith('sw_vers'):
            return '15.0'
        return 'nominal'

    monkeypatch.setattr(b.platform, 'system', lambda: 'Darwin')
    monkeypatch.setattr(b, 'command', command)
    monkeypatch.setattr(b.shutil, 'disk_usage', lambda _: SimpleNamespace(free=100 * 2**30))
    return state, InstalledProbe(SimpleNamespace(call=call))


@async_test
async def test_installed_fresh_admission_and_live_sensors(host):
    state, probe = host
    snapshot = await probe('/tmp')
    assert snapshot['blockers'] == []
    b.safe(snapshot, dict(min_headroom_pct=20, disk_floor_gb=10))
    state['pressure'] = '4'
    with pytest.raises(b.Refused, match='unsafe_snapshot'):
        b.safe(await probe('/tmp'), dict(min_headroom_pct=20, disk_floor_gb=10))
    assert state['admissions'] == 2


@async_test
async def test_standalone_unchanged(host):
    snapshot = await b.probe('/tmp')
    assert snapshot['blockers'] == ['processes_present', 'services_loaded']


@async_test
@pytest.mark.parametrize('field,value', [
    ('system', 'io.nocturne.worker\nio.nocturne.llama\n'),
    ('system', 'io.nocturne.worker-extra\n'),
    ('optional', 'io.nocturne.worker\n'),
    ('optional', 'io.nocturne.tunnel\n'),
    ('extra', '99 10 python3 -m worker.agent\n'),
])
async def test_unknown_competitors_block(host, field, value):
    state, probe = host
    state[field] = value
    assert (await probe('/tmp'))['blockers']


@async_test
async def test_identity_mismatch_refuses(host):
    state, probe = host
    state['mismatch'] = True
    with pytest.raises(b.Refused, match='process_identity'):
        await probe('/tmp')


@async_test
async def test_reused_worker_pid_refuses(host):
    state, probe = host
    state['start'] = 'Tue Jan 2 12:00:00 2026'
    with pytest.raises(b.Refused, match='process_identity'):
        await probe('/tmp')


@async_test
async def test_runner_identity_is_pinned(host):
    state, probe = host
    await probe('/tmp')
    state['runner_start'] = 'Tue Jan 2 12:00:00 2026'
    with pytest.raises(b.Refused, match='installed_admission'):
        await probe('/tmp')


@async_test
async def test_llama_must_be_disabled(host):
    state, probe = host
    state['disabled'] = '"io.nocturne.llama" => false'
    with pytest.raises(b.Refused, match='llama_not_disabled'):
        await probe('/tmp')


@async_test
async def test_admission_failure_redacted(host):
    _, probe = host
    probe.service.call = lambda _: {'worker_pid': True}
    with pytest.raises(b.Refused, match='^installed_admission$'):
        await probe('/tmp')
