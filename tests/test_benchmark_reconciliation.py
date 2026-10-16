# SPDX-License-Identifier: AGPL-3.0-or-later
"""Distributed Apply crash windows: real watchdog, durable fixture, HTTP faults."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import subprocess
import sys
import threading

import httpx
import pytest

from worker import benchmark_executor as e
from test_benchmark_executor import adapter, apply_job, binding, intent_race


class Coordinator:
    def __init__(self, adapter):
        self.adapter = adapter
        self.job = apply_job(adapter)
        self.calls = []
        self.offline = False
        self.lose_terminal = False
        self.terminal_attempted = False
        self.reject_terminal = False

    def client(self):
        return e.WorkerHTTP('http://localhost', 'fixture', 'rotated-credential',
                            transport=httpx.MockTransport(self.respond))

    def respond(self, req):
        body = json.loads(req.content)
        route = req.url.path.rsplit('/', 1)[-1]
        self.calls.append(route)
        if self.offline:
            raise httpx.ReadError('injected')
        assert req.headers['Authorization'] == 'Bearer rotated-credential'
        if route == 'lease':
            assert body['apply_protocol'] == 1
            return httpx.Response(200, json=dict(job=self.job, action='apply',
                lease='original-lease', recovery_only=self.job.get('apply_phase') == 'prepared'))
        assert 'apply_protocol' not in body
        if route == 'reconcile':
            r = body['reconcile']
            assert r['transaction_id'] == self.job.apply_request.transaction_id
            assert r['generation'] == 1 and r['durable'] is True
            assert body['lease'] == 'original-lease'
            record = e.load(self.adapter.root / 'journal')
            assert record['phase'] == r['phase']
            assert self.adapter.state()['fenced']
            phase = r['phase']
            old = self.job.get('apply_phase')
            if phase != 'prepared':
                self.terminal_attempted = True
                if self.reject_terminal:
                    self.offline = True
                    raise httpx.ReadError('before persistence')
            if old == phase:
                return httpx.Response(200, json=self.job)
            if old in ('applied', 'rolled_back') or phase == 'applied' and old != 'prepared':
                return httpx.Response(409)
            self.job['sequence'] += 1
            self.job['apply_phase'] = phase
            self.job['progress'] = 'validating' if phase == 'prepared' else 'done'
            if phase != 'prepared':
                self.job.update(result=phase, state='completed' if phase == 'applied' else 'failed',
                    restored=phase == 'rolled_back', failure='' if phase == 'applied' else 'execution')
                if self.lose_terminal:
                    self.offline = True
                    raise httpx.ReadError('after persistence')
        return httpx.Response(200, json=self.job)


@pytest.mark.parametrize('mode', ['complete', 'fail'])
@pytest.mark.parametrize('persisted', [True, False])
def test_restart_replays_irrevocable_decision(adapter, mode, persisted):
    server = Coordinator(adapter)
    server.lose_terminal = persisted
    server.reject_terminal = not persisted
    client = server.client()
    with pytest.raises(e.Refused):
        client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter, mode=mode))
    client.close()
    assert server.terminal_attempted
    phase = 'applied' if mode == 'complete' else 'rolled_back'
    assert e.load(adapter.root / 'journal')['phase'] == phase
    snapshot = adapter.snapshot()
    # Even explicit networkless recovery must never undo either decision.
    assert e.recover(adapter.root)['phase'] == phase
    assert adapter.snapshot() == snapshot and adapter.state()['fenced']
    assert not adapter.admit()
    server.offline = server.lose_terminal = server.reject_terminal = False
    server.calls.clear()
    client = server.client()
    result = client.poll_fixture(e.FixtureExecutor(adapter.root))
    assert result.result == phase and adapter.snapshot() == snapshot
    assert 'lease' not in server.calls  # terminal jobs are no longer leased
    assert ('reconcile' in server.calls) is not persisted
    assert not adapter.state()['fenced']
    assert adapter.admit()
    client.close()


@pytest.mark.parametrize('window', ['private_ack', 'private_commit', 'agreement', 'release'])
def test_restart_durable_write_windows(adapter, monkeypatch, window):
    server = Coordinator(adapter)
    original_atomic = e.atomic
    original_release = e.FilesystemFixture.release
    fired = False

    def atomic(path, value):
        nonlocal fired
        hit = (window == 'private_ack' and path.name == 'commit_ack' or
               window == 'private_commit' and path.name == 'journal' and value.get('phase') == 'applied' or
               window == 'agreement' and path.name == 'journal' and value.get('agreement'))
        if hit and not fired:
            fired = True
            if window != 'private_ack':
                original_atomic(path, value)  # crash after replace/fsync
            raise OSError('injected')
        return original_atomic(path, value)

    def release(self):
        nonlocal fired
        if window == 'release' and not fired:
            fired = True
            raise OSError('injected')
        return original_release(self)

    # private_commit fault is in recovery interpreter, after lost private ack.
    def lost_ack(path, value):
        if window == 'private_commit' and path.name == 'commit_ack':
            raise OSError('injected')
        return atomic(path, value)

    monkeypatch.setattr(e, 'atomic', lost_ack)
    monkeypatch.setattr(e.FilesystemFixture, 'release', release)
    client = server.client()
    try:
        client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter))
    except OSError:
        pass
    assert fired
    monkeypatch.setattr(e, 'atomic', original_atomic)
    monkeypatch.setattr(e.FilesystemFixture, 'release', original_release)
    if adapter.state()['fenced']:
        result = client.poll_fixture(e.FixtureExecutor(adapter.root))
        assert result.result == 'applied'
    assert adapter.snapshot().config != 'original'
    assert not adapter.state()['fenced']
    client.close()


@pytest.mark.parametrize('fault', ['health', 'opposite', 'identity', 'missing', 'journal'])
def test_terminal_status_never_suffices_without_matching_local_health(adapter, fault):
    server = Coordinator(adapter)
    server.lose_terminal = True
    client = server.client()
    with pytest.raises(e.Refused):
        client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter))
    server.offline = server.lose_terminal = False
    if fault == 'health':
        adapter.change(healthy=False)
    elif fault == 'opposite':
        server.job.update(apply_phase='rolled_back', result='rolled_back', state='failed',
                          failure='execution', restored=True)
    elif fault == 'identity':
        server.job['apply_request']['transaction_id'] = 'e' * 64
    elif fault == 'missing':
        client.close()
        client = e.WorkerHTTP('http://localhost', 'fixture', 'secret',
            transport=httpx.MockTransport(lambda _: httpx.Response(404)))
    else:
        record = e.load(adapter.root / 'journal')
        del record['transaction_id']
        e.atomic(adapter.root / 'journal', record)
    snapshot = adapter.snapshot()
    with pytest.raises(e.Refused):
        client.poll_fixture(e.FixtureExecutor(adapter.root))
    assert adapter.snapshot() == snapshot and adapter.state()['fenced']
    assert not adapter.admit()
    client.close()


@pytest.mark.parametrize('field,value', [('transaction_id', 'A' * 64), ('transaction_id', ''),
    ('generation', True), ('generation', 2), ('generation', 0)])
def test_transaction_wire_validation(adapter, field, value):
    job = apply_job(adapter)
    job['apply_request'][field] = value
    with pytest.raises(e.Refused, match='invalid_job'):
        e.validate_job(job, 'fixture')


@pytest.mark.parametrize('authorized', [True, False])
def test_fresh_interpreter_reconciles_prepared_journal_over_http(adapter, authorized):
    server = Coordinator(adapter)
    with pytest.raises(e.Refused, match='recovery_required'):
        e.FixtureExecutor(adapter.root).apply(server.job, binding(adapter),
                                             lease='original-lease', accept=lambda: False)
    assert e.load(adapter.root / 'journal')['phase'] == 'prepared'
    if authorized:
        server.job.update(apply_phase='prepared', sequence=1, progress='validating')
    else:
        server.job.update(state='recovery_required', progress='restoring', failure='restart')

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers['Content-Length']))
            response = server.respond(httpx.Request('POST', 'http://localhost' + self.path,
                content=body, headers={'Authorization': self.headers['Authorization']}))
            self.send_response(response.status_code)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(response.content)))
            self.end_headers()
            self.wfile.write(response.content)

    http = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=http.serve_forever, daemon=True)
    thread.start()
    expected = 'applied' if authorized else 'rolled_back'
    code = ('import sys; from worker.benchmark_executor import WorkerHTTP,FixtureExecutor; '
            'c=WorkerHTTP(sys.argv[2], "fixture", "rotated-credential"); '
            'r=c.poll_fixture(FixtureExecutor(sys.argv[1])); '
            'assert r.result == sys.argv[3]; c.close()')
    try:
        subprocess.run([sys.executable, '-c', code, str(adapter.root),
                        f'http://127.0.0.1:{http.server_port}', expected],
                       check=True, timeout=10, capture_output=True)
    finally:
        http.shutdown()
        http.server_close()
        thread.join(timeout=2)
    assert server.job.result == expected
    assert not adapter.state()['fenced']
    assert 'lease' not in server.calls


def test_intent_race_reconciliation_never_overwrites_newer_state(adapter, intent_race):
    server = Coordinator(adapter)
    client = server.client()
    try:
        with pytest.raises(e.Refused, match='recovery_required'):
            client.poll_fixture(e.FixtureExecutor(adapter.root), binding(adapter))
        for _ in range(2):
            server.calls.clear()
            with pytest.raises(e.Refused, match='recovery_required'):
                client.poll_fixture(e.FixtureExecutor(adapter.root))
            assert 'lease' not in server.calls
            assert 'reconcile' not in server.calls
            assert adapter.snapshot() == intent_race[0]
            assert adapter.state()['fenced'] and not adapter.admit()
            assert not e.load(adapter.root / 'journal')['restored']
    finally:
        client.close()


def test_prepared_recovery_lease_parses(adapter):
    server = Coordinator(adapter)
    server.job.update(apply_phase='prepared', sequence=1, progress='validating')
    client = server.client()
    assignment = client.lease()
    assert assignment.recovery_only and assignment.job.apply_request.generation == 1
    client.close()
