# SPDX-License-Identifier: AGPL-3.0-or-later
"""Disposable runner protocol tests; no installed service or subprocesses."""
from dataclasses import asdict
from types import SimpleNamespace
import time

import httpx
import pytest

from worker import benchmark_production as p
from worker.benchmark_executor import Job, PriorState, Assignment


@pytest.fixture
def evidence(monkeypatch, tmp_path):
    monkeypatch.setattr(p, 'STATE', tmp_path / 'production')
    job = Job(id='job', machine='worker', options=dict(context=8192, parallel=2,
        model_sha256='a'*64, build_digest='b'*64), provenance={}, summary={'quality': 'pass'},
        action='apply', apply_request=dict(transaction_id='c'*64, generation=1,
        completed_sequence=2, completed_at='completed', deadline_at='2099-01-01T00:00:00Z',
        requested_at='requested', audit=dict(actor='operator', code_revision='d'*40,
        config_generation='e'*64)))
    prior = PriorState('e'*64, 'configuration', True)
    record = dict(binding=p.binding(job), reported=True, prior=asdict(prior),
        verified_prior=asdict(prior), supervisor_digest='f'*64,
        result=dict(restored=True, summary=job['summary']), completed=dict(state='completed',
        sequence=2, finished_at='completed', summary=job['summary']))
    catalog = dict(config_generation='f'*64, presets={'1'*64: dict(model_sha256='a'*64,
        server_sha256='b'*64, ctx_size=16384, parallel=2, cache_type_k='q8_0', cache_type_v='q8_0')})
    service = SimpleNamespace(snapshot=lambda: prior,
        inventory=lambda: SimpleNamespace(code_revision='d'*40), call=lambda op: catalog)
    monkeypatch.setattr(p, 'validate_summary', lambda summary, options: None)
    return job, record, catalog, service


def test_exact_preset_and_distinct_generations(evidence):
    job, record, catalog, service = evidence
    assert p.apply_binding(job, record, service) == ('1'*64, 'f'*64)
    assert record['prior']['generation'] != record['supervisor_digest']


@pytest.mark.parametrize('field,value', [('ctx_size',8192), ('parallel',1),
    ('cache_type_k','f16'), ('cache_type_v','q4_0'), ('model_sha256','9'*64),
    ('server_sha256','9'*64)])
def test_preset_mismatch(evidence, field, value):
    job, record, catalog, service = evidence
    catalog['presets']['1'*64][field] = value
    with pytest.raises(p.Refused):
        p.apply_binding(job, record, service)


@pytest.mark.parametrize('change', ['supervisor', 'receipt', 'sequence', 'revision', 'quality'])
def test_stale_evidence(evidence, change, monkeypatch):
    job, record, catalog, service = evidence
    if change == 'supervisor': catalog['config_generation'] = '9'*64
    if change == 'receipt': record.pop('verified_prior')
    if change == 'sequence': record['completed']['sequence'] = 3
    if change == 'revision': service.inventory = lambda: SimpleNamespace(code_revision='9'*40)
    if change == 'quality':
        monkeypatch.setattr(p, 'validate_summary', lambda *a: (_ for _ in ()).throw(p.Refused('quality')))
    with pytest.raises(p.Refused):
        p.apply_binding(job, record, service)


def test_capability_default_and_explicit():
    bodies = []
    def handle(request):
        import json
        bodies.append(json.loads(request.content))
        return httpx.Response(200, json={'job': None})
    http = p.BenchmarkHTTP('http://localhost', 'worker', 'test',
            transport=httpx.MockTransport(handle))
    try:
        http.lease()
        http.production_apply = True
        http.lease()
    finally:
        http.close()
    assert 'apply_protocol' not in bodies[0]
    assert bodies[1]['apply_protocol'] == 1


def test_lost_coordinator_ack_queries_status(evidence):
    job, *_ = evidence
    agreed = dict(job, apply_phase='applied', result='applied')
    statuses = iter([job, agreed])
    calls = []
    def reconcile(*args):
        calls.append('reconcile')
        raise OSError('lost')
    http = SimpleNamespace(status=lambda _: next(statuses), reconcile=reconcile,
                           _matching_apply=lambda a,b: None)
    p.coordinator_agreement(http, Assignment(job, 'lease', False, 'apply'), 'applied')
    assert calls == ['reconcile']


@pytest.mark.parametrize('phase', ['applied', 'rolled_back', 'prepared', 'mutating', 'rolling_back', 'rollback_restoring'])
def test_child_recovery_never_reapplies(evidence, monkeypatch, phase):
    job, benchmark_record, _, _ = evidence
    path = p.journal_root() / 'apply.json'
    record = dict(job=dict(job), lease='lease', attempted=True, preset='1'*64,
                  supervisor_digest='f'*64)
    p.atomic(path, record)
    decision = dict(transaction_id='c'*64, generation=1, preset='1'*64,
                    original_config_digest='f'*64, phase=phase)
    terminal = phase if phase in ('applied', 'rolled_back') else 'rolled_back'
    commands = iter([{'command':'recover'}, {'agreement':terminal}])
    monkeypatch.setattr(p, 'pipe_line', lambda *a: next(commands))
    monkeypatch.setattr(p, 'emit', lambda value: None)
    calls = []
    def call(op, **kw):
        calls.append(op)
        if op == 'status': return decision
        if op == 'recover': return dict(decision, phase=terminal)
        assert op == 'finalize'
        assert kw['agreement'] == terminal
    service = SimpleNamespace(call=call, ping=lambda: {})
    assert p.apply_child(job, service) == 0
    assert 'prepare' not in calls and 'apply' not in calls
    assert p.load(path)['finalized'] is True


@pytest.mark.parametrize('agree', [True, False])
def test_child_prepare_agreement_precedes_apply(evidence, monkeypatch, agree):
    job, benchmark_record, _, inventory_service = evidence
    root = p.journal_root()
    p.atomic(root / 'journal.json', benchmark_record)
    p.atomic(root / 'apply.json', dict(job=dict(job), lease='lease'))
    calls = []
    messages = []
    commands = iter([{'command': 'start'},
                     {'agreement': 'prepared' if agree else 'rolled_back'},
                     {'agreement': 'applied'}])
    def receive(*args):
        command = next(commands)
        calls.append(command)
        return command
    monkeypatch.setattr(p, 'pipe_line', receive)
    monkeypatch.setattr(p, 'emit', messages.append)
    original_call = inventory_service.call
    def call(op, **kw):
        calls.append(op)
        if op == 'presets': return original_call(op)
        if op == 'prepare':
            assert kw == dict(preset='1'*64, transaction_id='c'*64, generation=1,
                              config_generation='f'*64)
            assert p.load(root / 'apply.json')['attempted'] is True
        if op in ('prepare', 'apply'):
            return dict(transaction_id='c'*64, generation=1, preset='1'*64,
                        original_config_digest='f'*64,
                        phase='prepared' if op == 'prepare' else 'applied')
        assert op == 'finalize'
    inventory_service.call = call
    inventory_service.ping = lambda: {}
    if agree:
        assert p.apply_child(job, inventory_service) == 0
        assert calls.index({'agreement': 'prepared'}) < calls.index('apply')
        assert calls.index({'agreement': 'applied'}) < calls.index('finalize')
    else:
        with pytest.raises(p.Refused): p.apply_child(job, inventory_service)
        assert 'apply' not in calls and 'finalize' not in calls
        assert not p.load(root / 'apply.json').get('finalized')


def test_pending_apply_bypasses_benchmark_reconciliation(evidence):
    job, record, _, _ = evidence
    record.update(job=dict(job), lease='lease')
    http = SimpleNamespace(status=lambda _: job)
    assert p.reconcile(http, p.journal_root() / 'unused', record) is True


def test_pipe_eof_and_deadline():
    import os
    read, write = os.pipe()
    os.close(write)
    try:
        with pytest.raises(p.Refused): p.pipe_line(read, time.monotonic()+1)
        with pytest.raises(p.Refused): p.pipe_line(read, time.monotonic()-1)
    finally:
        os.close(read)


@pytest.mark.parametrize('crash', ['before_prepare_send', 'prepare_response_lost'])
def test_ambiguous_prepare_recovery(evidence, monkeypatch, crash):
    job, benchmark_record, _, service = evidence
    root = p.journal_root()
    path = root / 'apply.json'
    p.atomic(root / 'journal.json', benchmark_record)
    p.atomic(path, dict(job=dict(job), lease='lease'))
    decision = dict(transaction_id='c'*64, generation=1, preset='1'*64,
                    original_config_digest='f'*64, phase='prepared')
    calls = []
    original_call = service.call
    def call(op, **kw):
        calls.append((op, kw))
        if op == 'presets': return original_call(op)
        if op == 'prepare': raise OSError('response lost')
        if op == 'status':
            return decision if crash == 'prepare_response_lost' else None
        if op in ('recover', 'abort'): return dict(decision, phase='rolled_back')
        assert op == 'finalize'
    service.call = call
    service.ping = lambda: {}
    monkeypatch.setattr(p, 'emit', lambda value: None)
    monkeypatch.setattr(p, 'pipe_line', lambda *a: {'command': 'start'})
    atomic = p.atomic
    def crash_write(path, value):
        atomic(path, value)
        if value.get('attempted'): raise OSError('crash before send')
    if crash == 'before_prepare_send': monkeypatch.setattr(p, 'atomic', crash_write)
    with pytest.raises(OSError): p.apply_child(job, service)
    monkeypatch.setattr(p, 'atomic', atomic)
    assert p.load(path)['attempted'] is True
    calls.clear()
    commands = iter([{'command': 'recover'}, {'agreement': 'rolled_back'}])
    monkeypatch.setattr(p, 'pipe_line', lambda *a: next(commands))
    assert p.apply_child(job, service) == 0
    assert [op for op, _ in calls] == ['status',
        'abort' if crash == 'before_prepare_send' else 'recover', 'finalize']
    if crash == 'before_prepare_send':
        assert calls[1][1] == dict(preset='1'*64, transaction_id='c'*64,
                                  generation=1, config_generation='f'*64)
    assert p.load(path)['finalized'] is True


@pytest.mark.parametrize('stale', [False, True])
def test_binding_refusal_uses_root_abort_not_fake_evidence(evidence, monkeypatch, stale):
    job, benchmark_record, _, service = evidence
    benchmark_record.pop('verified_prior')
    root = p.journal_root()
    p.atomic(root / 'journal.json', benchmark_record)
    path = root / 'apply.json'
    p.atomic(path, dict(job=dict(job), lease='lease'))
    original = service.call
    calls = []
    def call(op, **kw):
        calls.append(op)
        if op == 'presets': return original(op)
        if op == 'status': return dict(transaction_id='9'*64)
        if op == 'abort':
            assert kw['config_generation'] == 'f'*64
            if stale: raise p.Refused('apply_stale_config')
            return dict(transaction_id='c'*64, generation=1, preset='1'*64,
                        original_config_digest='f'*64, phase='rolled_back')
        assert op == 'finalize'
    service.call = call
    service.ping = lambda: {}
    commands = iter([{'command': 'start'}, {'agreement': 'rolled_back'}])
    monkeypatch.setattr(p, 'pipe_line', lambda *a: next(commands))
    monkeypatch.setattr(p, 'emit', lambda value: None)
    if stale:
        with pytest.raises(p.Refused, match='apply_stale_config'): p.apply_child(job, service)
        assert not p.load(path).get('finalized')
        assert p.load(path)['recovery_required'] == 'root_abort_original_binding'
        assert 'finalize' not in calls
    else:
        assert p.apply_child(job, service) == 0
        assert p.load(path)['phase'] == 'rolled_back'
    assert 'apply' not in calls and 'prepare' not in calls and 'release' not in calls
    assert p.load(root / 'journal.json') == benchmark_record


def test_abort_response_lost_retries_status_not_abort(evidence, monkeypatch):
    job, _, _, service = evidence
    path = p.journal_root() / 'apply.json'
    p.atomic(path, dict(job=dict(job), lease='lease', attempted=True,
                        preset='1'*64, supervisor_digest='f'*64))
    decision = dict(transaction_id='c'*64, generation=1, preset='1'*64,
                    original_config_digest='f'*64, phase='rolled_back')
    calls = []
    def call(op, **kw):
        calls.append(op)
        if op == 'status': return decision if 'abort' in calls else None
        if op == 'abort': raise OSError('lost abort response')
        assert op == 'finalize'
    service.call = call
    service.ping = lambda: {}
    monkeypatch.setattr(p, 'emit', lambda value: None)
    commands = iter([{'command': 'recover'}, {'command': 'recover'},
                     {'agreement': 'rolled_back'}])
    monkeypatch.setattr(p, 'pipe_line', lambda *a: next(commands))
    with pytest.raises(OSError): p.apply_child(job, service)
    assert not p.load(path).get('finalized')
    assert p.apply_child(job, service) == 0
    assert calls == ['status', 'abort', 'status', 'finalize']


@pytest.mark.parametrize('attempted', [False, True])
def test_crash_before_child_start_poll_routes_unattempted(evidence, monkeypatch, attempted):
    job, *_ = evidence
    pending = dict(job=dict(job), lease='lease', attempted=attempted)
    p.atomic(p.journal_root() / 'apply.json', pending)
    calls = []
    monkeypatch.setattr(p, 'launch_apply', lambda http, record, stop, **kw: calls.append(kw))
    p.poll_locked(SimpleNamespace(), None)
    assert calls == [dict(recovery=attempted)]


@pytest.mark.parametrize('state,phase,expired,expected', [
    ('running', '', False, 'start'), ('cancelling', '', False, 'recover'),
    ('running', 'prepared', False, 'recover'), ('running', '', True, 'recover')])
def test_restart_rechecks_coordinator_after_child_ready(evidence, monkeypatch,
                                                        state, phase, expired, expected):
    import io
    import json
    job, *_ = evidence
    events = []
    class Pipe(io.BytesIO):
        def fileno(self): return 42
        def close(self): pass
    child = SimpleNamespace(stdin=Pipe(), stdout=Pipe(), wait=lambda **kw: None)
    monkeypatch.setattr(p, 'verified_source', lambda: None)
    monkeypatch.setattr(p, 'validate_job', lambda *a: job)
    monkeypatch.setattr(p.subprocess, 'Popen', lambda *a, **kw: child)
    responses = iter([{'phase': 'ready'}, {'phase': 'finalized'}])
    def receive(*a):
        value = next(responses)
        events.append(value['phase'])
        return value
    monkeypatch.setattr(p, 'pipe_line', receive)
    if expired: monkeypatch.setattr(p, 'timestamp', lambda _: 0)
    def status(_):
        events.append('status')
        return dict(job, state=state, apply_phase=phase)
    http = SimpleNamespace(status=status, _matching_apply=lambda a,b: events.append('match'))
    p.launch_apply(http, dict(job=dict(job), lease='lease'),
                   SimpleNamespace(is_set=lambda: False), recovery=False)
    lines = child.stdin.getvalue().splitlines()
    assert json.loads(lines[1]) == {'command': expected}
    assert events == ['ready', 'status', 'match', 'finalized']
