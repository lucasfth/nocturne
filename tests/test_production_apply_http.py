# SPDX-License-Identifier: AGPL-3.0-or-later
"""Go-invoked disposable integration helper, NOT inference quality evidence.

Fake launchctl, peer attestation and protected-path checks; actual supervisor
bytes, policy, journals, ServiceClient/session wire IO and authenticated Go HTTP.
Run via TestBenchmarkProductionApplyHTTP, not as a standalone pytest suite.
"""
import contextlib
from dataclasses import asdict
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading

import httpx

from worker import benchmark_production as p
from worker import benchmark_service as s
from worker.benchmark_executor import Update


def run(root, worker_url, admin_url, mode):
    p.STATE = root / 'runner'
    s.protected = lambda path, **kw: Path(path)
    s.INSTALL_ROOT = root
    s.SUPERVISOR = root / 'bin/llama-supervisor.sh'
    s.APPLY_BASELINE = root / 'original'
    s.APPLY_CATALOG = root / 'catalog.json'
    (root / 'bin').mkdir()
    (root / 'models').mkdir()
    script = Path('worker/install-services.sh').read_bytes()
    original = script.split(b"llama-supervisor.sh >/dev/null <<'EOF'\n", 1)[1].split(b'\nEOF', 1)[0] + b'\n'
    s.SUPERVISOR.write_bytes(original)
    s.SUPERVISOR.chmod(0o755)
    s.APPLY_BASELINE.write_bytes(original)
    model = root / 'models/Ornith-1.5-9B-Q4_K_M.gguf'
    server = root / 'bin/llama-server'
    model.write_bytes(b'disposable-model')
    server.write_bytes(b'disposable-server')
    model.chmod(0o444)
    server.chmod(0o555)
    # Deliberately different from baseline: actual config transition required.
    entry = dict(model_sha256=s.file_digest(model), server_sha256=s.file_digest(server),
                 ctx_size=16384, parallel=2, cache_type_k='q8_0', cache_type_v='q8_0')
    s.durable(s.APPLY_CATALOG, dict(version=1, supervisor_sha256=s.file_digest(s.SUPERVISOR), presets=[entry]))
    cfg = s.InstallationConfig(1, 501, 600, s.Inventory(str(model.parent), str(server), 'd'*40),
                              True, 'b'*64, True, s.file_digest(s.APPLY_CATALOG))
    state = root / 'service'
    state.mkdir(mode=0o700)
    peer = dict(pid=123, uid=501, start_sec=1, start_usec=2)
    s.runner_peer = lambda conn: peer
    s.runner_absent = lambda old: True
    class Launchctl:
        def __init__(self):
            self.states = {label: dict(loaded=True, disabled=False) for label in s.LABELS}
            self.calls = []
        def state(self): return json.loads(json.dumps(self.states))
        def loaded(self, label): return self.states[label]['loaded']
        def mutate(self, verb, label):
            assert label == s.LLAMA
            assert json.loads((state / 'admission.json').read_text())['active']
            self.calls.append((verb, label))
            self.states[label]['disabled' if verb in ('enable', 'disable') else 'loaded'] = verb in ('disable', 'bootstrap')
    lc = Launchctl()
    prior_states = lc.state()
    healthy = True
    def make():
        return s.ProductionApplyAdapter(state, lc, lambda: 'c'*64, policy=s.ApplyPolicy(cfg),
            health=lambda: healthy, closed=lambda port: True, absent=lambda: True,
            readiness_timeout=0.2, readiness_interval=0.001)
    adapter = make()
    errors = []
    crash = False
    class Crash(BaseException): pass
    @contextlib.contextmanager
    def connection():
        left, right = socket.socketpair()
        client = object.__new__(s.ServiceClient)
        client.conn, client.prior = left, None
        left.settimeout(5)
        def serve():
            try:
                s.session(right, adapter, 15, cfg.inventory, lambda: True)
            except (EOFError, OSError, s.Refused, Crash) as exc:
                errors.append(type(exc).__name__)
        thread = threading.Thread(target=serve)
        thread.start()
        try: yield client
        finally:
            left.close()
            thread.join(5)
            assert not thread.is_alive()
    http = p.BenchmarkHTTP(worker_url, 'w', 'worker')
    http.production_apply = True
    with httpx.Client(headers={'Authorization': 'Bearer admin'}, timeout=5) as admin:
        base = admin_url + '/admin/machines/w/benchmarks'
        response = admin.post(base, json={'advanced': {'context': 8192, 'parallel': 2}})
        assert response.status_code == 202
        assignment = http.lease()
        assert assignment and not assignment.action
        job = assignment.job
        # SYNTHETIC completed prerequisite: no model execution/quality claim.
        o = job['options']
        summary = dict(quality='pass', context=o['context'], parallel=o['parallel'],
            prompt_tokens=o['context']-100, output_tokens=100, duration_seconds=2,
            tokens_per_second=50, model_verified=True,
            model_sha256=o['model_sha256'], build_digest=o['build_digest'])
        with connection() as client:
            prior = asdict(client.snapshot())
        completed = http.update(assignment, Update(job['sequence']+1, 'completed', 'done',
                                                  restored=True, summary=summary))
        p.atomic(p.journal_root() / 'journal.json', dict(binding=p.binding(job), reported=True,
            prior=prior, verified_prior=prior, supervisor_digest=s.file_digest(s.SUPERVISOR),
            result=dict(restored=True, summary=summary), completed=dict(completed)))
        response = admin.post(base+'/'+job['id']+'/apply', json={})
        assert response.status_code == 202
        assignment = http.lease()
        assert assignment.action == 'apply'
        job = assignment.job
        path = p.journal_root() / 'apply.json'
        p.atomic(path, dict(job=dict(job), lease=assignment.lease))
        # Harmless real child exits before any root mutation. Not model inference.
        subprocess.run([sys.executable, '-c', 'pass'], check=True, timeout=5)
        latest = None
        recovery = False
        dropped = False
        operations = []
        original_reconcile = http.reconcile
        def reconcile(a, phase):
            nonlocal dropped
            value = original_reconcile(a, phase)
            if mode == 'lost_coordinator_prepared_ack' and phase == 'prepared' and not dropped:
                dropped = True
                raise OSError('injected lost HTTP acknowledgement')
            return value
        http.reconcile = reconcile
        def emit(value):
            nonlocal latest
            latest = value['phase']
        def receive(fd, deadline, heartbeat=None):
            if latest == 'ready': return {'command': 'recover' if recovery else 'start'}
            if not (recovery and mode == 'lost_finalize_ack'):
                assert json.loads(adapter.fence_path.read_text())['active']
            p.coordinator_agreement(http, assignment, latest)
            if latest == 'prepared':
                assert http.status(job['id'])['state'] == 'running'
            return {'agreement': latest}
        p.emit, p.pipe_line = emit, receive
        real_replace = os.replace
        def replace(src, dst, *args, **kw):
            nonlocal crash
            if Path(dst) == s.SUPERVISOR and not crash:
                crash = True
                if mode == 'crash_after_rename': real_replace(src, dst, *args, **kw)
                raise Crash()
            return real_replace(src, dst, *args, **kw)
        real_recover = adapter.recover
        # Emulate daemon death: no finally recovery after the injected rename
        # boundary. Restart below rereads actual on-disk journal/config bytes.
        adapter.recover = lambda: None if crash else real_recover()
        with connection() as client:
            real_call = client.call
            def call(op, **kw):
                nonlocal healthy, dropped
                operations.append(op)
                if op == 'apply' and mode == 'health_failure': healthy = False
                if op == 'apply' and mode.startswith('crash_'): os.replace = replace
                value = real_call(op, **kw)
                if ((mode == 'lost_prepared_ack' and op == 'prepare') or
                        (mode == 'lost_finalize_ack' and op == 'finalize')) and not dropped:
                    dropped = True
                    raise OSError('injected lost root acknowledgement')
                return value
            client.call = call
            try:
                assert p.apply_child(job, client) == 0
            except (OSError, EOFError, s.Refused):
                assert mode in ('health_failure', 'lost_prepared_ack', 'lost_finalize_ack',
                                'crash_before_rename', 'crash_after_rename')
            finally:
                os.replace = real_replace
        if not p.load(path).get('finalized'):
            if mode != 'lost_finalize_ack':
                assert json.loads(adapter.fence_path.read_text())['active']
                assert adapter.journal.exists()
            if mode == 'health_failure':
                assert s.SUPERVISOR.read_bytes() == original
            if crash:
                expected = adapter.policy.presets[next(iter(adapter.policy.presets))] if mode == 'crash_after_rename' else original
                assert s.SUPERVISOR.read_bytes() == expected
            healthy = True
            adapter = make()
            adapter.recover()
            if mode != 'lost_finalize_ack':
                assert json.loads(adapter.fence_path.read_text())['active']
                assert adapter.status()['phase'] == 'rolled_back'
            recovery = True
            with connection() as client:
                recovery_call = client.call
                def recover_call(op, **kw):
                    assert op not in ('prepare', 'apply')
                    return recovery_call(op, **kw)
                client.call = recover_call
                assert p.apply_child(job, client) == 0
        expected_phase = 'applied' if mode in ('success', 'lost_finalize_ack', 'lost_coordinator_prepared_ack') else 'rolled_back'
        terminal = http.status(job['id'])
        assert terminal['apply_phase'] == terminal['result'] == expected_phase
        assert terminal['summary'] == summary
        assert p.load(path)['finalized'] is True
        assert adapter.status()['phase'] == expected_phase
        assert not json.loads(adapter.fence_path.read_text())['active']
        assert lc.state() == prior_states and lc.calls
        assert s.SUPERVISOR.read_bytes() == (original if expected_phase == 'rolled_back' else
            original.replace(s.APPLY_FLAGS, b'--ctx-size 16384 --parallel 2 -ctk q8_0 -ctv q8_0'))
        assert s.SUPERVISOR.stat().st_mode & 0o777 == 0o755
        assert operations.count('prepare') == 1
        assert operations.count('apply') == (0 if mode == 'lost_prepared_ack' else 1)
        assert dropped == mode.startswith('lost_')
        assert crash == mode.startswith('crash_')
        if mode in ('success', 'lost_finalize_ack', 'lost_coordinator_prepared_ack'):
            assert all(error == 'EOFError' for error in errors)
        (root / 'result.json').write_text(json.dumps(dict(id=job['id'], phase=expected_phase)))
    http.close()


if __name__ == '__main__':
    root = Path(sys.argv[1])
    def diagnostic(kind, value, tb):
        frames = []
        while tb:
            frames.append(str(tb.tb_lineno))
            tb = tb.tb_next
        (root / 'diagnostic').write_text(kind.__name__ + ':' + ','.join(frames))
    sys.excepthook = diagnostic
    run(root, *sys.argv[2:])
