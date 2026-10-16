# SPDX-License-Identifier: AGPL-3.0-or-later
"""Disposable files/harmless children only. No inference or production access."""
import asyncio
from dataclasses import asdict
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

import httpx
import pytest

from worker import benchmark_executor as e


@pytest.fixture
def adapter(tmp_path):
    return e.FilesystemFixture.initialize(tmp_path / 'fixture')


def wait_for(fn, timeout=5):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        value = fn()
        if value:
            return value
        time.sleep(.02)
    raise AssertionError('timed out')


def test_real_process_restore(adapter):
    prior = adapter.snapshot()
    result = e.FixtureExecutor(adapter.root).run()
    assert result['restored'] and result['phase'] == 'restored'
    assert adapter.snapshot() == prior
    assert not adapter.state()['fenced']
    assert adapter.admit()
    for path in adapter.root.iterdir():
        assert path.stat().st_mode & 0o077 == 0


@pytest.mark.parametrize('mode,cancel,budget,failure', [
    ('wait', lambda: False, .12, 'deadline'),
    ('wait', lambda: True, 2, 'execution'),
    ('fail', lambda: False, 2, 'execution'),
])
def test_deadline_cancel_failure(adapter, mode, cancel, budget, failure):
    started = time.monotonic()
    result = e.FixtureExecutor(adapter.root).run(mode=mode, cancel=cancel, budget_seconds=budget)
    assert result['restored'] and result['pgid'] is None
    assert result['failure'] == failure
    assert time.monotonic() - started < 4


def test_physical_drain_required(adapter):
    adapter.change(active=1)
    with pytest.raises(e.Refused, match='recovery_required'):
        e.FixtureExecutor(adapter.root).run()
    record = e.load(adapter.root / 'journal')
    assert record['pgid'] is None and not record['restored']
    assert adapter.state()['fenced']
    assert not adapter.admit()
    adapter.change(active=0)
    assert e.recover(adapter.root)['restored']


def test_independent_watchdog_survives_executor_kill(adapter):
    code = ('from worker.benchmark_executor import FixtureExecutor; '
            'import sys; FixtureExecutor(sys.argv[1]).run(mode="wait", budget_seconds=10)')
    parent = subprocess.Popen([sys.executable, '-c', code, str(adapter.root)],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        record = wait_for(lambda: (r if (r := e.load(adapter.root / 'journal'))['phase'] == 'running' else None))
        pgid = record['pgid']
        parent.kill()
        parent.wait(timeout=2)
        wait_for(lambda: e.load(adapter.root / 'journal').get('restored'))
        assert e.group_absent(pgid)
        assert not adapter.state()['fenced']
    finally:
        if parent.poll() is None:
            parent.kill()
            parent.wait()


def interrupted(adapter, **extra):
    prior = adapter.snapshot()
    record = dict(phase='applying', prior=asdict(prior), restored=False, pgid=None, **extra)
    e.atomic(adapter.root / 'journal', record)
    adapter.fence()
    adapter.change(config='partial', generation=e.digest(b'partial'), service=False, healthy=False)
    return prior


def test_restart_restores_interrupted_apply(adapter):
    prior = interrupted(adapter)
    assert not adapter.admit()
    # Fresh process, not a finally in the executor.
    subprocess.run([sys.executable, '-c',
                    'import sys; from worker.benchmark_executor import recover; recover(sys.argv[1])',
                    str(adapter.root)], check=True, timeout=5)
    assert adapter.snapshot() == prior
    assert e.load(adapter.root / 'journal')['restored']
    assert e.recover(adapter.root)['restored']


def test_unknown_launch_identity_stays_fenced(adapter):
    interrupted(adapter, launch_pending=True)
    with pytest.raises(e.Refused, match='process_identity_uncertain'):
        e.recover(adapter.root)
    assert adapter.state()['fenced']
    assert not e.load(adapter.root / 'journal')['restored']


def test_verify_failure_never_claims_restore(adapter, monkeypatch):
    interrupted(adapter)
    monkeypatch.setattr(e.FilesystemFixture, 'verify', lambda *args: False)
    with pytest.raises(e.Refused, match='restoration'):
        e.recover(adapter.root)
    assert adapter.state()['fenced']
    assert not e.load(adapter.root / 'journal')['restored']


def test_lock_blocks_concurrent_execution_and_admission(adapter):
    with e.exclusive(adapter.root):
        with pytest.raises(e.Refused, match='busy'):
            adapter.admit()
        with pytest.raises(e.Refused, match='recovery_required'):
            e.FixtureExecutor(adapter.root).run()


def request(adapter, **changes):
    values = dict(transaction_id='operator-action-1', expected_generation=adapter.snapshot().generation,
                  expected_model_sha256=e.digest(b'model'), expected_build_digest=e.digest(b'build'), config='new')
    values.update(changes)
    return e.Apply(**values)


def test_apply_explicit_commit_replay_and_stale_refusal(adapter):
    r = request(adapter)
    assert e.apply_fixture(adapter.root, r, model=b'model', build=b'build')['phase'] == 'applied'
    assert adapter.snapshot().config == 'new'
    assert not adapter.state()['fenced']
    with pytest.raises(e.Refused, match='replay'):
        e.apply_fixture(adapter.root, r, model=b'model', build=b'build')
    stale = e.Apply(**(asdict(r) | {'transaction_id': 'second'}))
    with pytest.raises(e.Refused, match='stale_apply'):
        e.apply_fixture(adapter.root, stale, model=b'model', build=b'build')
    assert e.recover(adapter.root)['phase'] == 'restored'
    assert adapter.snapshot().config == 'original'


def test_failed_apply_rolls_back_and_verifies(adapter):
    prior = adapter.snapshot()
    with pytest.raises(e.Refused, match='apply_failed_rolled_back'):
        e.apply_fixture(adapter.root, request(adapter), model=b'model', build=b'build', fail_health=True)
    assert adapter.snapshot() == prior
    assert e.load(adapter.root / 'journal')['restored']
    assert not adapter.state()['fenced']


def test_apply_digest_mismatch_no_mutation(adapter):
    prior = adapter.state()
    with pytest.raises(e.Refused, match='stale_apply'):
        e.apply_fixture(adapter.root, request(adapter), model=b'wrong', build=b'build')
    assert adapter.state() == prior


def test_private_storage_corruption_and_symlink_refused(adapter, tmp_path):
    (adapter.root / 'journal').chmod(0o644)
    with pytest.raises(e.Refused, match='private_file'):
        e.recover(adapter.root)
    (adapter.root / 'journal').unlink()
    outside = tmp_path / 'outside'
    outside.write_text('{}')
    (adapter.root / 'journal').symlink_to(outside)
    with pytest.raises(OSError):
        e.recover(adapter.root)
    assert outside.read_text() == '{}'
    with pytest.raises(FileExistsError):
        e.FilesystemFixture.initialize(adapter.root)
    with pytest.raises(e.Refused, match='private_file'):
        e.FilesystemFixture(tmp_path)


def test_atomic_fsync_and_uncertain_write_fences(adapter, monkeypatch):
    calls = []
    real = os.fsync
    monkeypatch.setattr(os, 'fsync', lambda fd: (calls.append(os.fstat(fd).st_mode), real(fd))[1])
    e.atomic(adapter.root / 'check', {'ok': True})
    assert len(calls) == 2
    interrupted(adapter)
    monkeypatch.setattr(e, 'atomic', lambda *a: (_ for _ in ()).throw(OSError('private error')))
    with pytest.raises(OSError):
        e.recover(adapter.root)
    assert not e.load(adapter.root / 'journal')['restored']


def job():
    return dict(id='job-1', machine='fixture', options=dict(context=32768, parallel=2, budget_seconds=30,
                model_sha256=e.digest(b'model'), build_digest=e.digest(b'build')),
                provenance=dict(schema_version=1, policy_version=1, fixture_version=1, worker_id='fixture',
                    model_sha256=e.digest(b'model'), build_digest=e.digest(b'build'),
                    created_at='2025-01-01T00:00:00Z',
                    deadline_at='2099-01-01T00:00:00Z'), state='running', sequence=0,
                progress='preparing', failure='', restored=False, updated_at='2025-01-01T00:00:00Z')


def test_http_poll_fixture_never_fabricates_completion(adapter, monkeypatch):
    monkeypatch.setenv('HTTP_PROXY', 'http://invalid.invalid:1')
    j = job()
    j['provenance'].update(code_revision='a' * 40,
                           config_generation=adapter.snapshot().generation)
    requests = []
    def respond(req):
        body = json.loads(req.content)
        requests.append((req.url.path, body))
        assert req.headers['authorization'] == 'Bearer secret'
        assert body['worker_id'] == 'fixture'
        if req.url.path.endswith('/lease'):
            return httpx.Response(200, json=dict(job=j, lease='opaque', recovery_only=False))
        if req.url.path.endswith('/update'):
            update = body['update']
            assert update['state'] == 'failed' and update['restored'] is True
            assert 'summary' not in update
            assert update['sequence'] == 1
            return httpx.Response(200, json=j | update)
        return httpx.Response(200, json=j)
    client = e.WorkerHTTP('http://localhost:8701', 'fixture', 'secret', transport=httpx.MockTransport(respond))
    try:
        result = client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter))
        assert result['restored']
        assert any(path.endswith('/status') for path, _ in requests)
    finally:
        client.close()


def test_recovery_only_does_not_spawn_or_ack_unrelated_journal(adapter, monkeypatch):
    j = job() | {'state': 'recovery_required'}
    updates = []
    def respond(req):
        if req.url.path.endswith('/lease'):
            return httpx.Response(200, json=dict(job=j, lease='opaque', recovery_only=True))
        if req.url.path.endswith('/update'):
            updates.append(json.loads(req.content)['update'])
        return httpx.Response(200, json=j)
    monkeypatch.setattr(e.FixtureExecutor, 'run', lambda *a, **k: pytest.fail('recovery spawned'))
    client = e.WorkerHTTP('http://localhost', 'fixture', 'secret', transport=httpx.MockTransport(respond))
    try:
        client.poll_fixture(e.FixtureExecutor(adapter.root))
        assert updates[0]['state'] == 'recovery_required'
        assert not updates[0]['restored']
    finally:
        client.close()


@pytest.mark.parametrize('response,code', [
    (httpx.Response(401, text='secret'), 'http_refused'),
    (httpx.Response(302, headers={'location': 'http://invalid.invalid'}), 'http_refused'),
    (httpx.Response(200, content=b'x' * 16385), 'response_limit'),
    (httpx.Response(200, content=b'{"job":null,"job":null}'), 'invalid_json'),
])
def test_http_redacted_bounded(response, code):
    client = e.WorkerHTTP('http://localhost', 'fixture', 'secret', transport=httpx.MockTransport(lambda _: response))
    try:
        with pytest.raises(e.Refused, match=code):
            client.lease()
    finally:
        client.close()


def test_conversion_real_python_exercise_rows(monkeypatch):
    from worker import model_benchmark as mb
    # Use the actual foundation artifact row producer with a disposable fake
    # backend. These are NOT claims of real model performance or quality.
    async def tokens(backend, target, suffix):
        return [1] * target
    class Backend:
        async def request(self, endpoint, payload):
            return {'timings': dict(prompt_n=len(payload['prompt']), predicted_n=128,
                prompt_ms=100, predicted_ms=2000, prompt_per_second=100, predicted_per_second=64)}
    monkeypatch.setattr(mb, 'prompt_tokens', tokens)
    options = job()['options']
    profile = dict(context=32768, parallel=2)
    rows = asyncio.run(mb.exercise(Backend(), dict(tokens=128, repeats=2), profile))
    profile.update(passed=True, quality={name: True for name in mb.FIXTURES}, measurements=rows)
    artifact = dict(status='completed', selected='candidate', cleanup=dict(
        owned_processes_stopped=True, owned_temp_removed=True, final_safety_verified=True), candidates=[dict(
        id='candidate', status='passed', profiles=[profile])])
    def convert():
        return e.measurement_summary(artifact, profile, options, model_bytes=b'model',
                                     build_bytes=b'build', output_allowance=128)
    result = convert()
    assert result['prompt_tokens'] == 32768 - 128 - 8
    assert result['output_tokens'] == 128
    assert result['duration_seconds'] == 2
    assert result['tokens_per_second'] == 64
    assert result['model_sha256'] == e.digest(b'model')
    row = next(r for r in rows if r['length'] == 'long' and not r['warmup'])
    for field, bad in [('decode_s', float('nan')), ('decode_tps', 999), ('prompt_tokens', 32768)]:
        original = row[field]
        row[field] = bad
        with pytest.raises(e.Refused, match='invalid_evidence'):
            convert()
        row[field] = original
    options['build_digest'] = e.digest(b'version string is not build')
    with pytest.raises(e.Refused, match='invalid_evidence'):
        convert()


def test_cancel_ack_matches_go_contract(adapter):
    j = job()
    updates = []
    def respond(req):
        if req.url.path.endswith('/lease'):
            return httpx.Response(200, json=dict(job=j, lease='opaque', recovery_only=False))
        if req.url.path.endswith('/update'):
            update = json.loads(req.content)['update']
            updates.append(update)
            return httpx.Response(200, json=j | update)
        return httpx.Response(200, json=j | {'state': 'cancelling'})
    client = e.WorkerHTTP('http://localhost', 'fixture', 'secret', transport=httpx.MockTransport(respond))
    try:
        client.poll_fixture(e.FixtureExecutor(adapter.root))
        assert updates == [dict(sequence=1, state='cancelled', progress='done', failure='', restored=True)]
    finally:
        client.close()


def test_apply_rollback_failure_remains_fenced(adapter, monkeypatch):
    monkeypatch.setattr(e.FilesystemFixture, 'verify', lambda *args: False)
    with pytest.raises(e.Refused, match='restoration'):
        e.apply_fixture(adapter.root, request(adapter), model=b'model', build=b'build')
    assert adapter.state()['fenced']
    assert not e.load(adapter.root / 'journal')['restored']
    assert not adapter.admit()


@pytest.mark.parametrize('mutation', [
    lambda j: j['options'].update(parallel=True),
    lambda j: j['provenance'].update(schema_version=True),
    lambda j: j['provenance'].update(deadline_at='2099-01-01'),
    lambda j: j.update(machine='other'),
    lambda j: j['options'].update(command='not-allowed'),
    lambda j: j['provenance'].update(build_digest='0' * 64),
])
def test_invalid_typed_jobs(mutation):
    j = job()
    mutation(j)
    with pytest.raises(e.Refused, match='invalid_job'):
        e.validate_job(j, 'fixture')


def test_already_expired_deadline_never_launches(adapter):
    record = e.FixtureExecutor(adapter.root).run(deadline_at=time.time() - 10)
    assert record['failure'] == 'deadline'
    assert record['restored'] and record['pgid'] is None


def apply_job(adapter, **changes):
    j = job()
    audit = dict(actor='admin-bearer', code_revision='a' * 40,
                 config_generation=adapter.snapshot().generation)
    j['provenance'].update(audit)
    j.update(action='apply', apply_request=dict(audit=audit.copy(),
        requested_at='2099-01-01T00:00:00Z', deadline_at='2099-01-01T00:00:30Z',
        completed_at='2025-01-01T00:00:00Z', completed_sequence=1,
        transaction_id='f' * 64, generation=1),
        summary=dict(quality='pass', context=32768, parallel=2, prompt_tokens=32640,
            output_tokens=128, duration_seconds=2, tokens_per_second=64,
            model_verified=True, model_sha256=e.digest(b'model'), build_digest=e.digest(b'build')))
    j.update(changes)
    return e.validate_job(j, 'fixture')


@pytest.fixture
def intent_race(adapter, monkeypatch):
    """Change configuration after durable intent, before the real watchdog."""
    original = e.subprocess.Popen
    snapshots = []

    def spawn(*args, **kwargs):
        assert e.load(adapter.root / 'journal')['phase'] == 'apply_pending'
        adapter.change(config='newer', generation=e.digest(b'newer'))
        snapshots.append(adapter.snapshot())
        return original(*args, **kwargs)

    monkeypatch.setattr(e.subprocess, 'Popen', spawn)
    return snapshots


def test_apply_intent_race_preserves_newer_state(adapter, intent_race):
    with pytest.raises(e.Refused, match='recovery_required'):
        e.FixtureExecutor(adapter.root).apply(apply_job(adapter), binding(adapter))
    assert adapter.snapshot() == intent_race[0]
    record = e.load(adapter.root / 'journal')
    assert record['phase'] == 'recovery_required'
    assert record['mutation_started'] is False and not record['restored']
    with pytest.raises(e.Refused, match='process_identity_uncertain'):
        e.recover(adapter.root)
    assert adapter.snapshot() == intent_race[0]
    assert adapter.state()['fenced'] and not adapter.admit()


def binding(adapter, **changes):
    values = dict(model=b'model', build=b'build', code_revision='a' * 40,
                  config_generation=adapter.snapshot().generation, apply_enabled=True)
    return e.FixtureBinding(**(values | changes))


def bridge(j, *, recovery=False, status=None):
    updates = []
    current = dict(j)
    def respond(req):
        nonlocal current
        if req.url.path.endswith('/lease'):
            assert json.loads(req.content)['apply_protocol'] == 1
            return httpx.Response(200, json=dict(job=current, action='apply', lease='opaque', recovery_only=recovery))
        if req.url.path.endswith('/reconcile'):
            u = json.loads(req.content)['reconcile']
            phase = u['phase']
            if phase == 'applied' and current.get('apply_phase') != 'prepared':
                return httpx.Response(409)
            current.update(apply_phase=phase, sequence=current['sequence'] + 1,
                           progress='validating' if phase == 'prepared' else 'done')
            if phase != 'prepared':
                current.update(result=phase, state='completed' if phase == 'applied' else 'failed',
                               restored=phase == 'rolled_back', failure='' if phase == 'applied' else 'execution')
                updates.append(dict(action='apply', result=phase, restored=current['restored'],
                                    state=current['state'], failure=current['failure']))
            return httpx.Response(200, json=current)
        if req.url.path.endswith('/update'):
            update = json.loads(req.content)['update']
            updates.append(update)
            current.update(update)
        return httpx.Response(200, json=(status() if status and not current.get('apply_phase') else current))
    return e.WorkerHTTP('http://localhost', 'fixture', 'secret', transport=httpx.MockTransport(respond)), updates


@pytest.mark.parametrize('mode,result', [('complete', 'applied'), ('fail', 'rolled_back')])
def test_typed_apply_bridge(adapter, mode, result):
    j = apply_job(adapter)
    assert isinstance(j, e.Job)
    assert isinstance(j.apply_request, e.ApplyRequest)
    assert isinstance(j.apply_request.audit, e.Audit)
    prior = adapter.snapshot()
    client, updates = bridge(j)
    try:
        response = client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter, mode=mode))
        assert response.result == result
        assert updates[0]['action'] == 'apply' and 'summary' not in updates[0]
        assert updates[0]['restored'] == (mode == 'fail')
        if mode == 'complete':
            assert json.loads(adapter.snapshot().config) == dict(context=32768, parallel=2)
        else:
            assert adapter.snapshot() == prior
        assert not adapter.state()['fenced']
    finally:
        client.close()


@pytest.mark.parametrize('changes', [dict(apply_enabled=False), dict(model=b'wrong'),
    dict(build=b'wrong'), dict(code_revision='b' * 40), dict(config_generation='b' * 64), None])
def test_apply_local_binding_refuses_without_mutation(adapter, changes):
    prior = adapter.state()
    client, updates = bridge(apply_job(adapter))
    try:
        b = binding(adapter, **changes) if changes is not None else None
        client.poll_fixture(e.FixtureExecutor(adapter.root), b)
        assert updates[0]['state'] == 'recovery_required'
        assert 'result' not in updates[0]
        assert adapter.state() == prior
        assert e.load(adapter.root / 'journal') == {'phase': 'idle'}
    finally:
        client.close()


def test_apply_recovery_after_commit_never_rolls_back(adapter, monkeypatch):
    j, b = apply_job(adapter), binding(adapter)
    executor = e.FixtureExecutor(adapter.root)
    prior = adapter.snapshot()
    assert executor.apply(j, b, accept=lambda: True)['phase'] == 'applied'
    j.update(state='recovery_required', progress='restoring', failure='restart')
    client, updates = bridge(j, recovery=True)
    monkeypatch.setattr(executor, 'apply', lambda *a, **k: pytest.fail('reapplied'))
    try:
        with pytest.raises(e.Refused, match='recovery_required'):
            client.poll_fixture(executor)
        assert not updates
        assert adapter.state()['fenced']
        assert not adapter.admit()
        assert adapter.snapshot() != prior
    finally:
        client.close()
    j.update(state='running', progress='preparing', failure='')
    with pytest.raises(e.Refused, match='recovery_required'):
        e.FixtureExecutor(adapter.root).apply(j, b)


def test_apply_unknown_recovery_never_claims_rollback(adapter):
    j = apply_job(adapter, state='recovery_required', progress='restoring', failure='restart')
    client, updates = bridge(j, recovery=True)
    try:
        client.poll_fixture(e.FixtureExecutor(adapter.root))
        assert updates[0]['state'] == 'recovery_required'
        assert not updates[0]['restored'] and 'result' not in updates[0]
    finally:
        client.close()


def test_apply_cancel_before_dispatch_refused(adapter):
    j = apply_job(adapter)
    client, updates = bridge(j, status=lambda: j | {'state': 'cancelling', 'progress': 'restoring'})
    try:
        client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter))
        assert adapter.snapshot().config == 'original'
        assert e.load(adapter.root / 'journal')['phase'] == 'idle'
        assert updates[0]['state'] == 'recovery_required'
    finally:
        client.close()


def test_apply_cancel_during_mutation_rollback(adapter):
    j = apply_job(adapter)
    cancelled = False
    def status():
        nonlocal cancelled
        cancelled |= e.load(adapter.root / 'journal')['phase'] == 'applying'
        return j | ({'state': 'cancelling', 'progress': 'restoring'} if cancelled else {})
    client, updates = bridge(j, status=status)
    try:
        client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter, mode='wait'))
        assert updates[0]['result'] == 'rolled_back'
        assert updates[0]['state'] == 'failed' and updates[0]['failure'] == 'execution'
        assert adapter.snapshot().config == 'original'
    finally:
        client.close()


@pytest.mark.parametrize('mutation', [
    lambda j: j.update(action='execute'),
    lambda j: j.update(action=''),
    lambda j: j.update(result='unknown'),
    lambda j: j.update(result='applied'),
    lambda j: j['apply_request'].update(command='no'),
    lambda j: j['apply_request'].update(completed_sequence=True),
    lambda j: j['apply_request'].update(deadline_at='2099-01-01T00:00:31Z'),
    lambda j: j['apply_request'].update(requested_at='2099-01-01'),
    lambda j: j['apply_request']['audit'].update(code_revision='not-a-revision'),
    lambda j: j['apply_request']['audit'].update(config_generation='b' * 64),
    lambda j: j['apply_request']['audit'].update(actor='private/path'),
    lambda j: j['apply_request']['audit'].update(extra=True),
    lambda j: j['provenance'].pop('code_revision'),
    lambda j: j['summary'].update(command='no'),
    lambda j: j['summary'].update(model_verified=1),
    lambda j: j.update(restored=1),
])
def test_apply_strict_wire_fields(adapter, mutation):
    j = apply_job(adapter)
    mutation(j)
    with pytest.raises(e.Refused, match='invalid_job'):
        e.validate_job(j, 'fixture')


@pytest.mark.parametrize('action', [None, '', 'benchmark', 'unknown'])
def test_assignment_action_must_match(adapter, action):
    response = dict(job=apply_job(adapter), lease='opaque', recovery_only=False)
    if action is not None:
        response['action'] = action
    client = e.WorkerHTTP('http://localhost', 'fixture', 'secret',
        transport=httpx.MockTransport(lambda _: httpx.Response(200, json=response)))
    try:
        with pytest.raises(e.Refused, match='invalid_assignment_action'):
            client.lease()
    finally:
        client.close()


@pytest.mark.parametrize('changes', [dict(action=''), dict(result='unknown'), dict(restored=True),
    dict(summary={}), dict(state='failed'), dict(failure='execution'), dict(sequence=2), dict(sequence=0)])
def test_apply_update_strict_before_http(adapter, changes):
    assignment = e.Assignment(apply_job(adapter), 'opaque', False, 'apply')
    u = e.Update(**(dict(sequence=1, state='completed', progress='done', action='apply', result='applied') | changes))
    client = e.WorkerHTTP('http://localhost', 'fixture', 'secret',
        transport=httpx.MockTransport(lambda _: pytest.fail('invalid update sent')))
    try:
        with pytest.raises(e.Refused):
            client.update(assignment, u)
    finally:
        client.close()


@pytest.mark.parametrize('phase,mode', [('applying', 'wait'), ('prepared', 'complete')])
def test_apply_watchdog_survives_executor_death(adapter, phase, mode):
    j = apply_job(adapter)
    code = ('import json,sys,time; from worker.benchmark_executor import *; '
            'j=validate_job(json.loads(sys.argv[2]), "fixture"); '
            f'b=FixtureBinding(b"model",b"build","a"*40,j.apply_request.audit.config_generation,True,{mode!r}); '
            'FixtureExecutor(sys.argv[1]).apply(j,b,accept=lambda: time.sleep(60))')
    parent = subprocess.Popen([sys.executable, '-c', code, str(adapter.root), json.dumps(j)],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        wait_for(lambda: e.load(adapter.root / 'journal')['phase'] == phase)
        assert adapter.state()['fenced']
        parent.kill()
        parent.wait(timeout=2)
        if phase == 'applying':
            wait_for(lambda: e.load(adapter.root / 'journal').get('restored'))
            assert adapter.snapshot().config == 'original'
        else:
            time.sleep(.1)
            assert e.load(adapter.root / 'journal')['phase'] == 'prepared'
            assert adapter.snapshot().config != 'original'
        assert adapter.state()['fenced']
    finally:
        if parent.poll() is None:
            parent.kill()
            parent.wait(timeout=2)


def test_apply_watchdog_death_durable_restart(adapter, monkeypatch):
    j, b = apply_job(adapter), binding(adapter, mode='wait')
    real_popen = subprocess.Popen
    children = []
    def spawn(*args, **kwargs):
        child = real_popen(*args, **kwargs)
        children.append(child)
        return child
    monkeypatch.setattr(e.subprocess, 'Popen', spawn)
    def kill_watchdog():
        if e.load(adapter.root / 'journal')['phase'] == 'applying':
            children[0].kill()
        return False
    with pytest.raises(e.Refused, match='recovery_required'):
        e.FixtureExecutor(adapter.root).apply(j, b, cancel=kill_watchdog)
    assert adapter.state()['fenced']
    assert not adapter.admit()
    subprocess.run([sys.executable, '-c',
        'import sys; from worker.benchmark_executor import recover; recover(sys.argv[1])',
        str(adapter.root)], check=True, timeout=5)
    assert adapter.snapshot().config == 'original'
    with pytest.raises(e.Refused, match='recovery_required'):
        e.FixtureExecutor(adapter.root).apply(j, b)


def test_apply_expired_request_refuses(adapter):
    j = apply_job(adapter)
    j['apply_request'].update(requested_at='2025-01-01T00:00:00Z', deadline_at='2025-01-01T00:00:30Z')
    with pytest.raises(e.Refused, match='cancelled_or_expired'):
        e.FixtureExecutor(adapter.root).apply(j, binding(adapter))
    assert e.load(adapter.root / 'journal')['phase'] == 'idle'


def test_apply_local_generation_changed_after_binding(adapter):
    j, b = apply_job(adapter), binding(adapter)
    adapter.change(generation=e.digest(b'changed'))
    before = adapter.state()
    with pytest.raises(e.Refused, match='stale_binding'):
        e.FixtureExecutor(adapter.root).apply(j, b)
    assert adapter.state() == before


def test_apply_intent_survives_watchdog_spawn_failure(adapter, monkeypatch):
    j, b = apply_job(adapter), binding(adapter)
    monkeypatch.setattr(e.subprocess, 'Popen', lambda *a, **k: (_ for _ in ()).throw(OSError('unavailable')))
    with pytest.raises(OSError):
        e.FixtureExecutor(adapter.root).apply(j, b)
    assert not adapter.admit()  # durable journal fences even before local fence
    assert e.load(adapter.root / 'journal')['phase'] == 'apply_pending'
    client, updates = bridge(j | {'state': 'recovery_required', 'progress': 'restoring', 'failure': 'restart'}, recovery=True)
    try:
        client.poll_fixture(e.FixtureExecutor(adapter.root))
        assert updates[0]['result'] == 'rolled_back'
        assert adapter.snapshot().config == 'original'
    finally:
        client.close()


@pytest.mark.parametrize('outcome', ['cancel', 'lost', 'ambiguous', 'ack_failure', 'healthy'])
def test_apply_completion_arbitration(adapter, outcome, monkeypatch):
    if outcome == 'ack_failure':
        original_atomic = e.atomic

        def fail_ack(path, value):
            if path.name == 'commit_ack':
                raise OSError('injected ack failure')
            return original_atomic(path, value)

        monkeypatch.setattr(e, 'atomic', fail_ack)
    j = apply_job(adapter)
    current = j.copy()
    offered = []

    def respond(req):
        nonlocal current
        if req.url.path.endswith('/lease'):
            return httpx.Response(200, json=dict(job=j, action='apply', lease='opaque', recovery_only=False))
        if req.url.path.endswith('/reconcile'):
            u = json.loads(req.content)['reconcile']
            if u['phase'] == 'prepared':
                offered.append(u)
                assert e.load(adapter.root / 'journal')['phase'] == 'prepared'
                assert adapter.state()['fenced']
                with pytest.raises(e.Refused, match='busy'):
                    adapter.admit()
                if outcome == 'cancel':
                    current = j | dict(state='cancelling', progress='restoring')
                    return httpx.Response(409)
                if outcome == 'ambiguous':
                    return httpx.Response(200, json=j)
                current = j | dict(apply_phase='prepared', sequence=1, progress='validating')
                if outcome == 'lost':
                    raise httpx.ReadError('lost')
            else:
                phase = u['phase']
                assert e.load(adapter.root / 'journal')['phase'] == phase
                current = current | dict(apply_phase=phase, result=phase, sequence=2,
                    state='completed' if phase == 'applied' else 'failed', progress='done',
                    restored=phase == 'rolled_back', failure='' if phase == 'applied' else 'execution')
        return httpx.Response(200, json=current)

    client = e.WorkerHTTP('http://localhost', 'fixture', 'secret', transport=httpx.MockTransport(respond))
    try:
        result = client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter))
        expected = 'rolled_back' if outcome in ('cancel', 'ambiguous') else 'applied'
        assert result.result == expected
        assert len(offered) == 1
        record = e.load(adapter.root / 'journal')
        assert record['decision'] == expected and record['agreement']
        assert record['phase'] == ('restored' if expected == 'rolled_back' else 'applied')
        assert not adapter.state()['fenced']
        assert (adapter.snapshot().config == 'original') == (expected == 'rolled_back')
        if outcome == 'healthy':
            assert e.load(adapter.root / 'commit_ack')['commit_nonce'] == record['commit_nonce']
    finally:
        client.close()


@pytest.mark.parametrize('field,value', [('code_revision', 'b' * 40), ('config_generation', 'b' * 64)])
def test_benchmark_pinned_binding(adapter, field, value):
    j = job()
    j['provenance'].update(code_revision='a' * 40, config_generation=adapter.snapshot().generation)
    b = binding(adapter)
    b.check(e.validate_job(j, 'fixture'), adapter, apply=False)
    j['provenance'][field] = value
    with pytest.raises(e.Refused, match='stale_binding'):
        b.check(e.validate_job(j, 'fixture'), adapter, apply=False)


def test_apply_watchdog_deadline_during_acceptance(adapter):
    j = apply_job(adapter)
    deadline = int(time.time()) + 2
    j['apply_request'].update(
        requested_at=time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(deadline - 30)),
        deadline_at=time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(deadline)))

    def delayed_acceptance():
        assert adapter.state()['fenced']
        time.sleep(max(0, deadline - time.time()) + .2)
        return True  # Prepared survives deadline; recovery consults coordinator.

    with pytest.raises(e.Refused, match='recovery_required'):
        e.FixtureExecutor(adapter.root).apply(j, binding(adapter), accept=delayed_acceptance)
    assert adapter.snapshot().config != 'original'
    assert e.load(adapter.root / 'journal')['phase'] == 'prepared'
    assert adapter.state()['fenced']
    assert not adapter.admit()


def test_wire_budget_and_sequence_boundaries():
    j = job()
    j['options']['budget_seconds'] = 600
    e.validate_job(j, 'fixture')
    j['options']['budget_seconds'] = 601
    with pytest.raises(e.Refused):
        e.validate_job(j, 'fixture')
    j = job() | dict(sequence=10000)
    with pytest.raises(e.Refused):
        e.validate_job(j, 'fixture')
    j.update(state='failed', progress='done', failure='execution', restored=True)
    e.validate_job(j, 'fixture')
    assignment = e.Assignment(e.Job(job() | dict(sequence=9999)), 'opaque', False)
    with pytest.raises(e.Refused):
        e.validate_update(assignment, e.Update(10000, 'running', 'measuring'))
    e.validate_update(assignment, e.Update(10000, 'failed', 'done', 'execution', True))


def test_apply_rollback_unverified_keeps_fence(adapter):
    adapter.change(active=1)
    client, updates = bridge(apply_job(adapter))
    try:
        with pytest.raises(e.Refused, match='recovery_required'):
            client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter))
        assert updates[0]['state'] == 'recovery_required'
        assert not updates[0]['restored'] and 'result' not in updates[0]
        assert adapter.state()['fenced']
        assert not adapter.admit()
    finally:
        client.close()
