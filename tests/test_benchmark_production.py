# SPDX-License-Identifier: AGPL-3.0-or-later
import asyncio
import os
import signal
import sys
from types import SimpleNamespace

import pytest

from worker import benchmark_production as p
from worker import agent
from worker.benchmark_service import ServiceAdapter, LLAMA, LABELS


def test_legacy_and_partial_install(monkeypatch, tmp_path):
    root = tmp_path / 'installation'
    monkeypatch.setattr(p, 'ROOT', root)
    assert p.admission_allowed()
    root.mkdir()
    monkeypatch.setattr(p.ServiceClient, 'admission_fenced', lambda: True)
    assert not p.admission_allowed()
    monkeypatch.setattr(p.ServiceClient, 'admission_fenced', lambda: False)
    assert p.admission_allowed()


def test_no_capability_no_lease(monkeypatch):
    monkeypatch.setattr(p, 'verified_source', lambda: None)
    class Service:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def ping(self): return {'service_control': True, 'benchmark_execution': False}
    monkeypatch.setattr(p, 'ServiceClient', Service)
    p.poll_once(SimpleNamespace(lease=lambda: pytest.fail('must not lease')), None)


def test_source_checkout_not_installation():
    with pytest.raises(p.Refused):
        p.verified_source()


def test_agent_execute_fails_before_backend(monkeypatch):
    monkeypatch.setattr(agent, 'admission_allowed', lambda: False)
    a = agent.Agent('http://localhost', 'worker', 'secret', tokens_per_s=100)
    calls = []
    async def post(path, body): calls.append(path)
    a.post = post
    asyncio.run(a.execute({'request_id': 'r', 'lease_id': 'l'}))
    assert calls == ['/worker/fail']


def test_service_restoration_keeps_worker_and_tunnel(tmp_path, monkeypatch):
    from worker import benchmark_service as service_module
    monkeypatch.setattr(service_module, "runner_absent", lambda peer: True)
    class Launchctl:
        def __init__(self):
            self.states = {label: {'loaded': True, 'disabled': False} for label in LABELS}
            self.calls = []
        def state(self): return {k: dict(v) for k, v in self.states.items()}
        def loaded(self, label): return self.states[label]['loaded']
        def mutate(self, verb, label):
            self.calls.append((verb, label))
            assert label == LLAMA
            self.states[label][{'enable': 'disabled', 'disable': 'disabled',
                                'bootstrap': 'loaded', 'bootout': 'loaded'}[verb]] = verb in ('disable', 'bootstrap')
    lc = Launchctl()
    service = ServiceAdapter(tmp_path, lc, lambda: 'generation', health=lambda: True,
                             closed=lambda port: True, absent=lambda: True)
    service.peer = dict(pid=12345, uid=501, start_sec=100, start_usec=42)
    prior = service.snapshot()
    service.fence()
    assert service.drain().active == 0
    service.restore(prior)
    assert service.verify(prior)
    service.release()
    assert all(label == LLAMA for _, label in lc.calls)


def test_eof_watchdog_reaps_owned_harmless_process(monkeypatch):
    processes = []
    async def transaction(job, service):
        child = await asyncio.create_subprocess_exec(sys.executable, '-c',
                'import time; time.sleep(60)', start_new_session=True)
        processes.append(child)
        try:
            await child.wait()
        except asyncio.CancelledError:
            os.killpg(child.pid, signal.SIGKILL)
            await child.wait()
        return {'restored': True, 'summary': None}
    monkeypatch.setattr(p, 'transaction', transaction)
    async def run():
        r, w = os.pipe()
        try:
            task = asyncio.create_task(p.watchdog(
                {'options': {'budget_seconds': 30},
                 'provenance': {'deadline_at': '2099-01-01T00:00:00Z'}},
                SimpleNamespace(ping=lambda: None), r))
            while not processes:
                await asyncio.sleep(.01)
            os.close(w)
            w = None
            result = await asyncio.wait_for(task, 5)
            assert result['restored']
            assert processes[0].returncode is not None
        finally:
            os.close(r)
            if w is not None: os.close(w)
    asyncio.run(run())


def test_watchdog_real_backend_teardown(monkeypatch, tmp_path):
    server = tmp_path / 'llama-server'
    server.write_text('#!/bin/sh\nexec /bin/sleep 60\n')
    server.chmod(0o700)
    backends = []

    async def transaction(job, service):
        b = p.benchmark.Backend({'server': server}, {'path': tmp_path / 'model'},
                               {'context': 32768, 'parallel': 1, 'spec_n': 2}, str(tmp_path))
        async def health(*args): return {'status': 'ok'}
        b.request = health
        try:
            await b.start()
            backends.append(b)
            await asyncio.sleep(60)
        except asyncio.CancelledError:
            pass
        finally:
            clean = await b.stop()
        return {'restored': clean, 'summary': None}

    monkeypatch.setattr(p, 'transaction', transaction)
    async def run():
        r, w = os.pipe()
        try:
            task = asyncio.create_task(p.watchdog(
                {'options': {'budget_seconds': 30},
                 'provenance': {'deadline_at': '2099-01-01T00:00:00Z'}},
                SimpleNamespace(ping=lambda: None), r))
            while not backends:
                await asyncio.sleep(.01)
            os.close(w)
            w = None
            assert (await asyncio.wait_for(task, 5))['restored']
            assert backends[0].process.returncode is not None
        finally:
            os.close(r)
            if w is not None: os.close(w)
    asyncio.run(run())


def test_no_apply_advertisement():
    import httpx
    import json
    bodies = []
    def handle(request):
        bodies.append(json.loads(request.content))
        return httpx.Response(200, json={'job': None})
    http = p.BenchmarkHTTP('http://localhost', 'worker', 'test',
                           transport=httpx.MockTransport(handle))
    try:
        assert http.lease() is None
        assert 'apply_protocol' not in bodies[0]
    finally:
        http.close()


@pytest.mark.parametrize('context', [8192, 16384, 32768])
@pytest.mark.parametrize('parallel', [1, 2])
def test_production_profiles(context, parallel):
    config = {}
    p.profile_config(config, dict(context=context, parallel=parallel))
    assert config['profiles'] == [dict(context=context, parallel=parallel, spec_n=2)]


def test_production_profile_default_and_bounds():
    config = {}
    p.profile_config(config, {})
    assert config['profiles'] == [dict(context=32768, parallel=2, spec_n=2)]
    for options in [dict(context=8191), dict(context=32769), dict(parallel=3),
                    dict(parallel=True), dict(context=True), dict(context=8193),
                    dict(context=32767)]:
        with pytest.raises(p.Refused):
            p.profile_config({}, options)


def test_replay_requires_durable_matching_restoration():
    from dataclasses import asdict
    from worker.benchmark_executor import PriorState
    prior = PriorState('generation', 'configuration', True)
    job = dict(id='job', machine='worker', options={}, provenance={})
    service = SimpleNamespace(snapshot=lambda: prior)
    record = dict(binding=p.binding(job), prior=asdict(prior),
                  result=dict(restored=True, summary=None))
    assert not p.replay(job, record, service)['restored']
    record['verified_prior'] = asdict(prior)
    assert p.replay(job, record, service)['restored']
    service.snapshot = lambda: PriorState('different', 'configuration', True)
    assert not p.replay(job, record, service)['restored']


def test_recovery_never_launches(monkeypatch, tmp_path):
    from worker.benchmark_executor import Assignment
    tmp_path.chmod(0o700)
    monkeypatch.setattr(p, 'STATE', tmp_path / 'production')
    job = dict(id='job', machine='worker', options={}, provenance={},
               state='recovery_required', sequence=1)
    updates = []
    http = SimpleNamespace(lease=lambda: Assignment(job, 'private-lease', True),
                           status=lambda _: job, update=lambda *args: updates.append(args))
    monkeypatch.setattr(p, 'launch', lambda *args: pytest.fail('recovery must not run'))
    p.poll_locked(http, None)
    assert len(updates) == 1
    assert not (p.STATE / 'journal.json').exists()


@pytest.fixture
def pending_journal(monkeypatch, tmp_path):
    from contextlib import nullcontext
    from dataclasses import asdict
    from worker.benchmark_executor import PriorState
    tmp_path.chmod(0o700)
    monkeypatch.setattr(p, 'STATE', tmp_path / 'production')
    prior = PriorState('generation', 'config', True)
    job = dict(id='old', machine='worker', options={}, provenance={},
               state='running', sequence=1)
    record = dict(binding=p.binding(job), job=job, lease='pinned-lease',
                  started=True, prior=asdict(prior), verified_prior=asdict(prior),
                  result=dict(restored=True, summary=None))
    path = p.journal_root() / 'journal.json'
    p.atomic(path, record)
    monkeypatch.setattr(p, 'ServiceClient',
                        lambda: nullcontext(SimpleNamespace(snapshot=lambda: prior)))
    monkeypatch.setattr(p, 'launch', lambda *a: pytest.fail('must not rerun'))
    return path, job


def test_accepted_lost_report_then_next_job(pending_journal):
    from worker.benchmark_executor import Assignment
    path, job = pending_journal
    events = []
    next_job = dict(job, id='next', state='recovery_required')
    def status(key):
        events.append(('status', key))
        return job if key == 'old' else next_job
    def update(assignment, value):
        events.append(('update', assignment.job['id']))
        if assignment.job['id'] == 'old':
            assert assignment.lease == 'pinned-lease'
            job.update(state=value.state, sequence=value.sequence)
            raise OSError('response lost')
    def lease():
        events.append(('lease', 'next'))
        return Assignment(next_job, 'next-lease', True)
    http = SimpleNamespace(status=status, update=update, lease=lease)
    with pytest.raises(OSError):
        p.poll_locked(http, None)
    assert not p.load(path).get('reported')
    assert events == [('status', 'old'), ('update', 'old')]
    events.clear()
    p.poll_locked(http, None)
    assert events[:2] == [('status', 'old'), ('lease', 'next')]
    assert p.load(path)['reported'] is True


@pytest.mark.parametrize('failure', ['report', 'status', 'binding', 'receipt'])
def test_reconciliation_failures_keep_journal(pending_journal, failure):
    path, job = pending_journal
    if failure == 'receipt':
        record = p.load(path)
        record.pop('verified_prior')
        p.atomic(path, record)
        job['state'] = 'failed'
    before = path.read_bytes()
    def status(_):
        if failure == 'status':
            raise p.Refused('missing_status')
        return dict(job, options={'changed': True}) if failure == 'binding' else job
    def update(*args):
        raise OSError('report unavailable')
    http = SimpleNamespace(status=status, update=update,
                           lease=lambda: pytest.fail('must reconcile before lease'))
    for _ in range(2):
        with pytest.raises((p.Refused, OSError)):
            p.poll_locked(http, None)
        assert path.read_bytes() == before


def test_restarted_running_journal_reports_without_lease(pending_journal):
    path, job = pending_journal
    record = p.load(path)
    record.pop('verified_prior')
    p.atomic(path, record)
    updates = []
    http = SimpleNamespace(status=lambda _: job,
                           update=lambda a, u: updates.append((a, u)),
                           lease=lambda: pytest.fail('unrestored journal blocks lease'))
    p.poll_locked(http, None)
    assert updates[0][0].lease == 'pinned-lease'
    assert updates[0][1].state == 'recovery_required'
    assert not p.load(path).get('reported')


def test_transaction_stale_binding_before_fence_actual_service(monkeypatch, tmp_path):
    import json
    import socket
    import threading
    from worker import benchmark_service as s
    from worker.benchmark_executor import Job

    class Launchctl:
        def state(self):
            return {label: dict(loaded=True, disabled=False) for label in s.LABELS}

        def mutate(self, *args):
            pytest.fail('pre-fence failure must not mutate services')

    tmp_path.chmod(0o700)
    monkeypatch.setattr(p, 'STATE', tmp_path / 'production')
    root = tmp_path / 'service'
    root.mkdir(mode=0o700)
    adapter = s.ServiceAdapter(root, Launchctl(), lambda: 'current',
                               health=lambda: True)
    client_sock, server_sock = socket.socketpair()
    client = object.__new__(s.ServiceClient)
    client.conn, client.prior = client_sock, None
    errors = []
    def serve():
        try:
            with server_sock:
                s.session(server_sock, adapter, 30,
                          s.Inventory('/unused', '/unused', 'revision'))
        except Exception as exc:
            errors.append(type(exc).__name__)
    thread = threading.Thread(target=serve)
    thread.start()
    job = Job(id='stale', machine='worker', options={},
              provenance=dict(config_generation='stale', code_revision='revision'))
    path = p.journal_root() / 'journal.json'
    p.atomic(path, dict(binding=p.binding(job), job=dict(job),
                        lease='lease', started=False))
    try:
        result = asyncio.run(p.transaction(job, client))
    finally:
        client.close()
        thread.join(timeout=5)
    assert not thread.is_alive()
    assert errors == []
    assert result == dict(restored=True, summary=None)
    record = p.load(path)
    assert record['verified_prior'] == record['prior']
    assert adapter.record is None
    assert json.loads(adapter.fence_path.read_text())['active'] is False


@pytest.mark.parametrize('apply_enabled', [False, True])
def test_transaction_adapter_explicit_ping(monkeypatch, tmp_path, apply_enabled):
    from dataclasses import asdict
    from worker.benchmark_executor import Job, PriorState
    monkeypatch.setattr(p, 'STATE', tmp_path / 'production')
    job = Job(id='job', machine='worker', options={},
              provenance=dict(config_generation='stale', code_revision='revision'))
    prior = PriorState('generation', 'configuration', True)
    calls = []
    def call(op):
        calls.append(op)
        assert op == 'presets' and apply_enabled
        return dict(config_generation='f'*64)
    # Production transaction requires ping; adapters must implement the real
    # capability contract rather than bypassing it with getattr/defaults.
    service = SimpleNamespace(snapshot=lambda: prior,
        ping=lambda: dict(service_control=True, benchmark_execution=True,
                          production_apply=apply_enabled), call=call,
        inventory=lambda: SimpleNamespace(code_revision='revision'),
        restore=lambda value: None, verify=lambda value: True, release=lambda: None)
    path = p.journal_root() / 'journal.json'
    p.atomic(path, dict(binding=p.binding(job), job=dict(job), lease='lease'))
    assert asyncio.run(p.transaction(job, service)) == dict(restored=True, summary=None)
    record = p.load(path)
    assert record['verified_prior'] == asdict(prior)
    assert record.get('supervisor_digest') == ('f'*64 if apply_enabled else None)
    assert calls == (['presets'] if apply_enabled else [])


def test_summary_digest_binding():
    options = dict(context=32768, parallel=1, budget_seconds=30,
                   model_sha256='a'*64, build_digest='b'*64)
    profile = dict(context=32768, parallel=1, passed=True,
                   quality={k: True for k in ('caller_recipient_direction', 'grounded_retrieval',
                                              'instruction_following', 'structured_json')},
                   measurements=[dict(length='long', warmup=False, prompt_tokens=32504,
                                      decode_tokens=16, decode_s=2, decode_tps=8)])
    artifact = dict(status='completed', selected='one',
                    cleanup={k: True for k in ('owned_processes_stopped', 'owned_temp_removed',
                                               'final_safety_verified')},
                    candidates=[dict(id='one', status='passed', profiles=[profile])])
    assert p.measurement_summary(artifact, profile, options, model_digest='a'*64,
                                 build_digest='b'*64)['output_tokens'] == 16
    with pytest.raises(p.Refused):
        p.measurement_summary(artifact, profile, options, model_digest='c'*64,
                              build_digest='b'*64)
