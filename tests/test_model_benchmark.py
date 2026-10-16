"""All execution tests use fake probes/backends; no inference or service operations."""
import asyncio
import json
from pathlib import Path
import sys

import pytest

from worker import model_benchmark as m


@pytest.fixture
def raw(tmp_path):
    server = tmp_path / 'server'
    server.write_text('unused')
    server.chmod(0o700)
    models = []
    for i in range(3):
        p = tmp_path / f'model{i}.gguf'
        p.write_bytes(b'GGUFfake')
        models.append({'path': p.name, 'parameters': (i + 1) * 1000, 'quantization': 'Q6_K'})
    return dict(server=server.name, runtime_s=30, tokens=16, repeats=2,
                disk_floor_gb=5, min_prefill_tps=100, min_decode_tps=10,
                min_headroom_pct=20, max_swap_growth_mb=0,
                profiles=[dict(context=8192, parallel=1), dict(context=16384, parallel=2)],
                candidates=models)


def load(tmp_path, raw):
    p = tmp_path / 'catalog.json'
    p.write_text(json.dumps(raw))
    return m.catalog(p)


def snapshot():
    return dict(chip='apple', ram_gb=32, available_pct=70, pressure=1,
                swap_mb=0, on_ac=True, battery_pct=100, idle_s=300,
                thermal='nominal', disk_gb=100, rss_mb=1000)


async def probes(*_):
    return snapshot()


class FakeBackend:
    instances = []
    failure = None
    pid = 123

    def __init__(self, c, model, profile, directory):
        self.model = model
        self.directory = directory
        self.stopped = False
        self.calls = []
        self.instances.append(self)

    async def start(self):
        if self.failure == 'start':
            raise OSError('/private/secret-host')
        if self.failure == 'hang':
            await asyncio.sleep(100)

    async def request(self, route, payload=None):
        if self.failure == 'request':
            raise OSError('private prompt')
        self.calls.append((route, payload))
        if route == '/apply-template':
            return {'prompt': '<system>' + payload['messages'][0]['content'] +
                    '<user>' + payload['messages'][1]['content'] + '<assistant>'}
        if route == '/tokenize':
            return {'tokens': list(payload['content'].encode())}
        n = len(payload['prompt'])
        speed = 5 if self.model['parameters'] == 3000 else 20
        text = bytes(payload['prompt']).decode()
        content = ('{"caller":"Mira","recipient":"Sol","direction":"outbound"}'
                   if 'Do not invert' in text else
                   'ready' if 'lowercase' in text else '{"color":"red","count":7}'
                   if 'JSON object' in text else 'IVORY-731')
        return {'content': content, 'timings': dict(prompt_n=n, predicted_n=payload['n_predict'],
                                prompt_ms=100, predicted_ms=200,
                                prompt_per_second=200, predicted_per_second=speed)}

    async def stop(self):
        self.stopped = True
        return True


@pytest.fixture(autouse=True)
def reset(monkeypatch):
    async def version(_):
        return '1234'
    monkeypatch.setattr(m, 'backend_version', version)
    FakeBackend.instances = []
    FakeBackend.failure = None


def run(c, probe=probes):
    report = m.plan(c)
    asyncio.run(m.execute(c, report, probe, FakeBackend))
    return report


@pytest.mark.parametrize('key,value', [
    ('runtime_s', 601), ('runtime_s', True), ('runtime_s', 10.0),
    ('tokens', 257), ('tokens', 0), ('min_decode_tps', float('nan')),
    ('min_prefill_tps', float('inf')), ('min_headroom_pct', 19),
    ('max_swap_growth_mb', -1), ('disk_floor_gb', 4), ('repeats', 1),
])
def test_numeric_catalog(tmp_path, raw, key, value):
    raw[key] = value
    with pytest.raises(m.Refused):
        load(tmp_path, raw)


@pytest.mark.parametrize('change', ['context', 'parallel', 'count', 'parameters', 'magic', 'extra'])
def test_catalog_limits(tmp_path, raw, change):
    if change == 'context':
        raw['profiles'][0]['context'] = 32769
    elif change == 'parallel':
        raw['profiles'][0]['parallel'] = 3
    elif change == 'count':
        raw['candidates'] *= 2
    elif change == 'parameters':
        raw['candidates'][0]['parameters'] = '7B'
    elif change == 'magic':
        (tmp_path / raw['candidates'][0]['path']).write_bytes(b'nope')
    else:
        raw['command'] = 'unsafe'
    with pytest.raises(m.Refused):
        load(tmp_path, raw)


@pytest.mark.parametrize('change', ['low_context', 'q4', 'weaken_quant', 'low_policy'])
def test_quality_policy_floor(tmp_path, raw, change):
    if change == 'low_context':
        raw['profiles'][0]['context'] = 4096
    elif change == 'q4':
        raw['candidates'][0]['quantization'] = 'Q4_K_M'
    elif change == 'weaken_quant':
        raw['allowed_quantizations'] = ['Q4_K_M']
    else:
        raw['min_context'] = 4095
    with pytest.raises(m.Refused):
        load(tmp_path, raw)


def test_no_candidate(tmp_path, raw):
    raw['min_decode_tps'] = 999
    report = run(load(tmp_path, raw))
    assert report['status'] == 'no_suitable_candidate'
    assert report['selected'] is None


def test_unsafe_baseline_saved(tmp_path, raw):
    async def unsafe(*_):
        s = snapshot()
        s['pressure'] = 2
        return s
    report = run(load(tmp_path, raw), unsafe)
    assert report['baseline']['pressure'] == 2
    assert report['reason'] == 'unsafe_snapshot'
    assert not FakeBackend.instances


def test_process_bounds():
    assert m.competing_process('bash -c "echo llama-server; python -m worker.agent"')
    assert not m.competing_process('python script.py --description llama-server')
    assert m.competing_process('/opt/bin/llama-server --model model.gguf')
    assert m.competing_process('/opt/python3 -m worker.agent')
    assert m.competing_process('/bin/bash /opt/bin/llama-supervisor.sh')


def test_hardware_unknown_and_oversize(tmp_path):
    assert m.hardware_fit({'path': tmp_path / 'missing'}, snapshot()) == 'unknown'
    p = tmp_path / 'large'
    with p.open('wb') as f:
        f.truncate(32 * 2**30)
    assert m.hardware_fit({'path': p}, snapshot()) == 'insufficient_ram'


def test_quality_failure_and_end_preservation(tmp_path, raw):
    c = load(tmp_path, raw)
    class Wrong(FakeBackend):
        async def request(self, route, payload=None):
            result = await super().request(route, payload)
            if route == '/completion':
                result['content'] = 'wrong'
                assert 8192 - c['tokens'] - 104 <= len(payload['prompt']) <= 8192 - c['tokens'] - 8
                text = bytes(payload['prompt']).decode()
                assert text.endswith('<assistant>')
                assert any(text.endswith(v[0] + '<assistant>') for v in m.FIXTURES.values())
                assert text.index('IVORY-731') < len(text) // 4
                assert len(text) // 3 < text.index('seven red bolts') < 2 * len(text) // 3
            return result
    b = Wrong(c, c['candidates'][0], c['profiles'][0], '')
    result = asyncio.run(m.quality_checks(b, c, c['profiles'][0]))
    assert not any(result.values())


def test_dryrun_loaded_services_keeps_capabilities(tmp_path, raw, monkeypatch):
    c = load(tmp_path, raw)
    async def loaded(*_):
        return dict(snapshot(), blockers=['services_loaded'], chip='Apple M2 Max')
    monkeypatch.setattr(m, 'probe', loaded)
    report = m.plan(c)
    asyncio.run(m.capabilities(c, report))
    assert report['baseline']['chip'] == 'Apple M2 Max'
    assert 'services_loaded' in report['blockers']
    assert not FakeBackend.instances


def test_optional_launchd_domain(monkeypatch):
    class Process:
        returncode = 113
        def __init__(self):
            self.stdout = asyncio.StreamReader()
            self.stdout.feed_eof()
            self.stderr = asyncio.StreamReader()
            self.stderr.feed_data(b'Could not find domain for gui/123')
            self.stderr.feed_eof()
        async def wait(self):
            return self.returncode
    async def spawn(*args, **kwargs):
        return Process()
    monkeypatch.setattr(m.asyncio, 'create_subprocess_exec', spawn)
    assert asyncio.run(m.command('launchctl', optional_domain=True)) == ''
    with pytest.raises(m.Refused):
        asyncio.run(m.command('launchctl'))
    Process.returncode = 64
    assert asyncio.run(m.command('launchctl', optional_domain=True)) == ''
    Process.returncode = 1
    with pytest.raises(m.Refused):
        asyncio.run(m.command('launchctl', optional_domain=True))


@pytest.mark.parametrize('code,diagnostic,accepted', [
    (125, b'Could not print domain: 125: Domain does not support specified action\n', True),
    (125, b'Could not print domain: 125: Domain does not support specified action: denied', False),
    (125, b'Permission denied', False),
    (1, b'Could not print domain: 125: Domain does not support specified action', False),
])
def test_optional_launchd_125(monkeypatch, code, diagnostic, accepted):
    class Process:
        returncode = code
        def __init__(self):
            self.stdout = asyncio.StreamReader()
            self.stdout.feed_eof()
            self.stderr = asyncio.StreamReader()
            self.stderr.feed_data(diagnostic)
            self.stderr.feed_eof()
        async def wait(self):
            return self.returncode
    async def spawn(*args, **kwargs):
        return Process()
    monkeypatch.setattr(m.asyncio, 'create_subprocess_exec', spawn)
    if accepted:
        assert asyncio.run(m.command('/bin/launchctl', optional_domain=True)) == ''
    else:
        with pytest.raises(m.Refused, match='probe_failed'):
            asyncio.run(m.command('/bin/launchctl', optional_domain=True))
    with pytest.raises(m.Refused, match='probe_failed'):
        asyncio.run(m.command('/bin/launchctl'))


@pytest.mark.parametrize('reverse', [False, True])
def test_grounded_call_roles(tmp_path, raw, reverse):
    c = load(tmp_path, raw)
    class Roles(FakeBackend):
        async def request(self, route, payload=None):
            result = await super().request(route, payload)
            if route == '/completion' and 'Do not invert' in bytes(payload['prompt']).decode():
                text = bytes(payload['prompt']).decode()
                assert text.index('caller Mira calls supplier Sol') < len(text) // 4
                assert 'Other call: Nia calls Theo' in text
                value = dict(caller='Sol' if reverse else 'Mira',
                             recipient='Mira' if reverse else 'Sol', direction='outbound')
                # Both answers satisfy the same key/type/direction schema.
                assert set(value) == {'caller', 'recipient', 'direction'}
                assert all(isinstance(v, str) for v in value.values())
                result['content'] = json.dumps(value)
            return result
    b = Roles(c, c['candidates'][0], c['profiles'][0], '')
    verdicts = asyncio.run(m.quality_checks(b, c, c['profiles'][0]))
    assert verdicts['caller_recipient_direction'] is (not reverse)
    assert all(v for k, v in verdicts.items() if k != 'caller_recipient_direction')


@pytest.mark.parametrize('value', [True, 7.0, '7'])
def test_structured_integer_not_coerced(value):
    assert not m.exact_structured({'count': value}, {'count': 7})


def test_generic_structured_values():
    assert m.exact_structured({'items': ['Mira', {'active': True}]},
                              {'items': ['Mira', {'active': True}]})
    assert not m.exact_structured({'extra': 1}, {})


def test_q4_explicit_optin(tmp_path, raw):
    raw['allowed_quantizations'] = ['Q4_K_M']
    for model in raw['candidates']:
        model['quantization'] = 'Q4_K_M'
    c = load(tmp_path, raw)
    assert c['profiles'][0]['spec_n'] == 2
    assert m.plan(c)['quantization_policy'] == ['Q4_K_M']


@pytest.mark.parametrize('value', [-1, 4, True, '2'])
def test_spec_enum(tmp_path, raw, value):
    raw['profiles'][0]['spec_n'] = value
    with pytest.raises(m.Refused):
        load(tmp_path, raw)


def test_process_metadata_spaces_and_quotes():
    assert m.competing_process('/Volumes/Model Disk/llama-server -m x',
                               '/Volumes/Model Disk/llama-server')
    assert m.competing_process('bash -e /Model Disk/llama-supervisor.sh', '/bin/bash')
    assert not m.competing_process("tool --label unmatched'", '/usr/bin/tool')


def test_probe_only_no_backend(tmp_path, monkeypatch):
    async def unsafe(*_):
        return dict(snapshot(), pressure=2, blockers=['services_loaded'])
    monkeypatch.setattr(m, 'probe', unsafe)
    def forbidden(*_):
        pytest.fail('no catalog/server calls')
    monkeypatch.setattr(m, 'catalog', forbidden)
    monkeypatch.setattr(m, 'backend_version', forbidden)
    p = tmp_path / 'inventory.json'
    assert m.main(['--probe-only', '--output', str(p)]) == 0
    r = json.loads(p.read_text())
    assert r['baseline']['pressure'] == 2
    assert 'unsafe_snapshot' in r['blockers']
    assert 'candidates' not in r


def test_whole_snapshot_timeout(monkeypatch):
    async def slow(*_):
        await asyncio.sleep(60)
    monkeypatch.setattr(m, '_probe', slow)
    with pytest.raises(m.Refused, match='snapshot_timeout'):
        asyncio.run(m.probe('/tmp'))


def test_duplicate_json():
    with pytest.raises(m.Refused):
        m.strict_json('{"a":1,"a":2}')


@pytest.mark.parametrize('key,value', [
    ('on_ac', False), ('battery_pct', 49), ('idle_s', 119), ('thermal', 'fair'),
    ('pressure', 2), ('available_pct', 19), ('disk_gb', 4),
    ('swap_mb', 1), ('ram_gb', float('nan')), ('idle_s', True),
])
def test_safety(tmp_path, raw, key, value):
    c = load(tmp_path, raw)
    s = snapshot()
    s[key] = value
    with pytest.raises(m.Refused):
        m.safe(s, c, snapshot())


def test_profiles_ranking_cleanup(tmp_path, raw):
    c = load(tmp_path, raw)
    report = run(c)
    assert report['status'] == 'completed'
    assert report['selected'] == 'candidate_2'
    assert [r['status'] for r in report['candidates']] == ['passed', 'passed', 'failed']
    assert len(FakeBackend.instances) == 6
    assert all(b.stopped and not Path(b.directory).exists() for b in FakeBackend.instances)
    assert report['cleanup'] == dict(owned_processes_stopped=True, owned_temp_removed=True,
                                    preexisting_state_modified=False, final_safety_verified=True)
    for row in report['candidates']:
        for p in row['profiles']:
            assert len(p['measurements']) == 2 * 3 * p['parallel']
            assert all(r['prompt_tokens'] + r['decode_tokens'] < p['context']
                       for r in p['measurements'])
    assert 'baseline' in report and 'final' in report


@pytest.mark.parametrize('failure', ['start', 'request'])
def test_failure_cleanup(tmp_path, raw, failure):
    FakeBackend.failure = failure
    report = run(load(tmp_path, raw))
    assert report['status'] == 'refused_or_failed'
    assert report['selected'] is None
    assert FakeBackend.instances[0].stopped
    assert not Path(FakeBackend.instances[0].directory).exists()
    assert report['candidates'][1]['status'] == 'skipped'
    assert 'private' not in json.dumps(report)


def test_deadline_cleanup(tmp_path, raw):
    c = load(tmp_path, raw)
    c['runtime_s'] = 6.02  # injectable short deadline, not a valid CLI catalog
    FakeBackend.failure = 'hang'
    report = run(c)
    assert report['status'] == 'timeout'
    assert FakeBackend.instances[0].stopped
    assert report['cleanup']['owned_temp_removed']


def test_monitor_abort(tmp_path, raw):
    calls = 0
    async def unsafe(*_):
        nonlocal calls
        calls += 1
        s = snapshot()
        if calls >= 3:
            s['available_pct'] = 10
        return s
    FakeBackend.failure = 'hang'
    report = run(load(tmp_path, raw), unsafe)
    assert report['status'] == 'refused_or_failed'
    assert report['cleanup']['owned_processes_stopped']
    assert not report['cleanup']['final_safety_verified']


def test_interrupt_cleanup(tmp_path, raw):
    c = load(tmp_path, raw)
    report = m.plan(c)
    FakeBackend.failure = 'hang'
    async def scenario():
        task = asyncio.create_task(m.execute(c, report, probes, FakeBackend))
        await asyncio.sleep(.02)
        task.cancel()
        await task
    asyncio.run(scenario())
    assert report['status'] == 'interrupted'
    assert FakeBackend.instances[0].stopped


def test_dryrun_private_sanitized(tmp_path, raw, monkeypatch):
    load(tmp_path, raw)
    def forbidden(*_):
        pytest.fail('dry run must not lock or start inference')
    monkeypatch.setattr(m, 'Lock', forbidden)
    monkeypatch.setattr(m, 'probe', probes)
    output = tmp_path / 'report.json'
    assert m.main(['--catalog', str(tmp_path / 'catalog.json'), '--output', str(output)]) == 0
    text = output.read_text()
    assert str(tmp_path) not in text and 'model0' not in text
    assert json.loads(text)['status'] == 'dry_run'
    assert output.stat().st_mode & 0o777 == 0o600
    assert m.main(['--catalog', str(tmp_path / 'catalog.json'), '--output', str(output)]) == 1
    assert output.read_text() == text


def test_invalid_catalog_never_overwrites(tmp_path):
    p = tmp_path / 'bad.json'
    p.write_text('invalid')
    assert m.main(['--catalog', str(p), '--output', str(p)]) == 1
    assert p.read_text() == 'invalid'


def test_lock_duplicate(tmp_path, monkeypatch):
    monkeypatch.setattr(m, 'LOCK_ROOT', tmp_path)
    with m.Lock():
        with pytest.raises(BlockingIOError):
            with m.Lock():
                pass
    with m.Lock():
        pass


def test_owned_subprocess_stop():
    # Harmless local child only; never invokes a model or macOS service tools.
    async def scenario():
        b = m.Backend({}, {}, {}, '')
        b.process = await asyncio.create_subprocess_exec(
            sys.executable, '-c', 'import time; time.sleep(60)', start_new_session=True)
        assert await b.stop()
        assert b.process.returncode is not None
    asyncio.run(scenario())


def test_partial_profiles_cannot_rank():
    rows = [dict(id='large', parameters=10, status='passed', profiles=[{}]),
            dict(id='small', parameters=5, status='passed', profiles=[{}, {}])]
    assert m.rank(rows, 2) == 'small'


def test_host_mismatch_sanitized(tmp_path, raw, monkeypatch):
    load(tmp_path, raw)
    monkeypatch.setattr(m.socket, 'gethostname', lambda: 'private-actual-name')
    output = tmp_path / 'result.json'
    assert m.main(['--catalog', str(tmp_path / 'catalog.json'), '--output', str(output),
                   '--execute', '--expected-hostname', 'private-expected-name']) == 1
    assert 'private' not in output.read_text()
    assert not FakeBackend.instances


def test_spawn_failure(tmp_path, raw, monkeypatch):
    c = load(tmp_path, raw)
    async def fail(*args, **kwargs):
        raise OSError('private executable error')
    monkeypatch.setattr(m.asyncio, 'create_subprocess_exec', fail)
    report = m.plan(c)
    asyncio.run(m.execute(c, report, probes, m.Backend))
    assert report['status'] == 'refused_or_failed'
    assert report['cleanup']['owned_processes_stopped']
    assert report['cleanup']['owned_temp_removed']
    assert 'private' not in json.dumps(report)


def test_cancel_during_cleanup(tmp_path, raw):
    c = load(tmp_path, raw)
    report = m.plan(c)
    async def scenario():
        entered = asyncio.Event()
        class SlowStop(FakeBackend):
            async def stop(self):
                entered.set()
                await asyncio.sleep(.02)
                self.stopped = True
                return True
        task = asyncio.create_task(m.execute(c, report, probes, SlowStop))
        await entered.wait()
        task.cancel()
        await task
    asyncio.run(scenario())
    assert report['status'] == 'interrupted'
    assert all(b.stopped for b in FakeBackend.instances)
    assert report['cleanup']['owned_temp_removed']


@pytest.mark.parametrize('blocked', ['service', 'process', 'missing', 'ps', 'ps_absolute', 'race', 'ps_impostor'])
def test_fresh_probe_failclosed(tmp_path, monkeypatch, blocked):
    monkeypatch.setattr(m.platform, 'system', lambda: 'Darwin')
    async def command(*args, **kwargs):
        key = args[-1]
        readings = {'machdep.cpu.brand_string': 'Apple M', 'hw.memsize': str(32 * 2**30),
                    'kern.memorystatus_level': '70', 'kern.memorystatus_vm_pressure_level': '1',
                    'vm.swapusage': 'used = 0.00M', 'batt': "'AC Power' 100%",
                    'IOHIDSystem': '"HIDIdleTime" = 300000000000'}
        if key in readings:
            return '' if blocked == 'missing' and key == 'batt' else readings[key]
        if args[0] == sys.executable:
            return 'nominal'
        if args[0] == '/bin/launchctl':
            return 'io.nocturne.worker' if blocked == 'service' else ''
        if args[-1] == 'pid=,rss=,args=' and blocked in {'ps', 'ps_absolute', 'race', 'ps_impostor'}:
            cmd = {'ps': 'ps -axo pid=,rss=,args=',
                   'ps_absolute': '/bin/ps -axo pid=,rss=,args=',
                   'race': '/usr/bin/new-process', 'ps_impostor': 'ps-impostor'}[blocked]
            return f'88 1024 {cmd}'
        if args[-1] == 'pid=,comm=':
            return '77 /usr/bin/python' if blocked == 'process' else ''
        return '77 1024 python -m worker.agent' if blocked == 'process' else ''
    monkeypatch.setattr(m, 'command', command)
    if blocked in {'race', 'ps_impostor'}:
        with pytest.raises(m.Refused, match='process_snapshot_race'):
            asyncio.run(m.probe(tmp_path))
    elif blocked in {'ps', 'ps_absolute'}:
        assert asyncio.run(m.probe(tmp_path))['blockers'] == []
    elif blocked == 'missing':
        with pytest.raises(m.Refused):
            asyncio.run(m.probe(tmp_path))
    else:
        s = asyncio.run(m.probe(tmp_path))
        assert s['blockers']
        with pytest.raises(m.Refused):
            m.safe(s, {'min_headroom_pct': 20, 'disk_floor_gb': 5})
