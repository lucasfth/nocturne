# SPDX-License-Identifier: AGPL-3.0-or-later
"""Bounded worker slice; NOT installed or wired into agent.py.

Trusted adapters must implement physical drain, a durable admission fence shared
with their inference dispatcher, snapshot/restore and independent verification.
Only the explicitly initialized disposable filesystem fixture is executable here.
No production adapter is registered. Local owner/ancestors and local POSIX fsync/
flock semantics are trusted. A lost watchdog with a possibly live group fails
closed: restart never signals a recycled PID. Operator reconciliation is required.

Apply uses durable prepared/applied/rolled_back reconciliation. Local applied is
irreversible, including after lost acknowledgments and restart. Admission remains
fenced until matching coordinator agreement AND independent local health proof.
"""
from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass, asdict
from datetime import datetime
import fcntl
import hashlib
import json
import math
import os
import re
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
from typing import Protocol

import httpx


class Refused(ValueError):
    """Only static redacted codes are public errors."""


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def strict(raw: bytes):
    def pairs(items):
        result = {}
        for k, v in items:
            if k in result:
                raise Refused('duplicate_key')
            result[k] = v
        return result
    if len(raw) > 1024 * 1024:
        raise Refused('size_limit')
    try:
        return json.loads(raw, object_pairs_hook=pairs,
                          parse_constant=lambda _: (_ for _ in ()).throw(Refused('nonfinite')))
    except (ValueError, UnicodeError):
        raise Refused('invalid_json') from None


def private_root(root):
    root = Path(root)
    s = root.lstat()
    if not stat.S_ISDIR(s.st_mode) or s.st_uid != os.getuid() or s.st_mode & 0o077:
        raise Refused('private_directory_required')
    return root


def load(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        s = os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or s.st_uid != os.getuid() or s.st_mode & 0o077:
            raise Refused('private_file_required')
        with os.fdopen(fd, 'rb', closefd=False) as f:
            return strict(f.read(1024 * 1024 + 1))
    finally:
        os.close(fd)


def atomic(path, value):
    """Private replacement, file AND directory durability before acknowledgement."""
    path = Path(path)
    private_root(path.parent)
    data = json.dumps(value, allow_nan=False, separators=(',', ':')).encode()
    if len(data) > 1024 * 1024:
        raise Refused('size_limit')
    fd, tmp = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
        d = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(d)
        finally:
            os.close(d)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


@contextmanager
def exclusive(root):
    fd = os.open(root / 'lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        s = os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or s.st_mode & 0o077:
            raise Refused('private_lock_required')
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise Refused('busy') from None
        yield
    finally:
        os.close(fd)


@dataclass(frozen=True)
class PriorState:
    generation: str
    config: str
    service: bool


@dataclass(frozen=True)
class DrainAck:
    generation: str
    active: int
    service_stopped: bool


class TrustedAdapter(Protocol):
    """Methods must be bounded; recovery must work in a separate interpreter.

    fence/drain and admission share a lock. verify must independently establish
    config generation/content, service health and absence of owned processes.
    A production installation must supply its own trusted watchdog binding; never
    deserialize an import, executable, command, environment or path from a job.
    """
    def snapshot(self) -> PriorState: ...
    def fence(self) -> None: ...
    def drain(self) -> DrainAck: ...
    def restore(self, prior: PriorState) -> None: ...
    def verify(self, prior: PriorState) -> bool: ...
    def release(self) -> None: ...


class FilesystemFixture:
    """Disposable test state only; does not control real services or inference."""
    def __init__(self, root):
        self.root = private_root(root)
        if load(self.root / 'fixture') != {'disposable': True, 'version': 1}:
            raise Refused('unsupported_adapter')

    @classmethod
    def initialize(cls, root):
        root = Path(root)
        root.mkdir(mode=0o700)  # must be NEW, never adopt existing runtime data
        parent = os.open(root.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(parent)
        finally:
            os.close(parent)
        atomic(root / 'fixture', {'disposable': True, 'version': 1})
        atomic(root / 'state', dict(config='original', generation=digest(b'original'),
                                   service=True, healthy=True, active=0, fenced=False))
        atomic(root / 'journal', {'phase': 'idle'})
        return cls(root)

    def state(self):
        return load(self.root / 'state')

    def change(self, **values):
        s = self.state()
        s.update(values)
        atomic(self.root / 'state', s)

    def admit(self):
        with exclusive(self.root):
            record = load(self.root / 'journal')
            if (record['phase'] not in ('idle', 'restored', 'applied', 'rolled_back') or
                    record.get('action') == 'apply' and not record.get('agreement')):
                return False
            s = self.state()
            if s['fenced'] or not s['healthy'] or not s['service']:
                return False
            self.change(active=s['active'] + 1)
            return True

    def snapshot(self):
        s = self.state()
        return PriorState(s['generation'], s['config'], s['service'])

    def fence(self):
        self.change(fenced=True)

    def drain(self):
        s = self.state()
        if not s['fenced'] or s['active'] != 0:
            raise Refused('not_drained')
        self.change(service=False)
        return DrainAck(s['generation'], 0, True)

    def restore(self, prior):
        self.change(config=prior.config, generation=prior.generation,
                    service=prior.service, healthy=True)

    def verify(self, prior):
        s = self.state()
        return (self.snapshot() == prior and s['healthy'] is True and
                s['active'] == 0 and s['fenced'] is True)

    def release(self):
        self.change(fenced=False)


def group_absent(pgid):
    if pgid is None:
        return True
    try:
        os.killpg(pgid, 0)
        return False
    except ProcessLookupError:
        return True


def restore_locked(adapter, record):
    adapter.fence()
    if record.get('action') == 'apply' and (record.get('phase') in ('applied', 'rolled_back') or
            record.get('decision') in ('applied', 'rolled_back')):
        return record  # irreversible local decision; reconciliation owns release
    # Never kill a persisted PID after restart: it could have been reused.
    if record.get('launch_pending') or not group_absent(record.get('pgid')):
        raise Refused('process_identity_uncertain')
    prior = PriorState(**record['prior'])
    adapter.restore(prior)
    if not adapter.verify(prior):
        raise Refused('restoration')
    if record.get('action') == 'apply':
        record.update(phase='rolled_back', restored=True)
        atomic(adapter.root / 'journal', record)
        return record
    uncertain = record.get('commit_uncertain', False)
    record.update(phase='recovery_required' if uncertain else 'restored', restored=True)
    if uncertain:
        record['recovery_reason'] = 'reconcile_coordinator_and_physical_state'
    atomic(adapter.root / 'journal', record)
    if not uncertain:
        adapter.release()
    return record


def recover(root):
    adapter = FilesystemFixture(root)
    with exclusive(adapter.root):
        record = load(adapter.root / 'journal')
        if record['phase'] == 'idle':
            return record  # absence of a transaction is NOT restoration proof
        return restore_locked(adapter, record)


@dataclass(frozen=True)
class Apply:
    """Trusted local fixture action; never accepts browser configuration."""
    transaction_id: str
    expected_generation: str
    expected_model_sha256: str
    expected_build_digest: str
    config: str


def apply_fixture(root, request: Apply, *, model: bytes, build: bytes, fail_health=False):
    """Explicit transaction, pinned content and generation, durable rollback.

    model/build are trusted local fixture bytes, never coordinator-supplied paths.
    Production must revalidate pinned open artifacts (see benchmark_discovery).
    """
    a = FilesystemFixture(root)
    with exclusive(a.root):
        previous = load(a.root / 'journal')
        if (previous['phase'] not in ('idle', 'restored', 'applied', 'rolled_back') or
                previous.get('action') == 'apply' and not previous.get('agreement')):
            raise Refused('recovery_required')
        if previous.get('transaction_id') == request.transaction_id:
            raise Refused('replay')
        prior = a.snapshot()
        if (prior.generation != request.expected_generation or
                digest(model) != request.expected_model_sha256 or
                digest(build) != request.expected_build_digest):
            raise Refused('stale_apply')
        target = PriorState(digest((prior.generation + request.transaction_id + request.config).encode()),
                            request.config, prior.service)
        record = dict(phase='applying', prior=asdict(prior), target=asdict(target),
                      transaction_id=request.transaction_id, restored=False, pgid=None)
        atomic(a.root / 'journal', record)
        try:
            a.fence()
            ack = a.drain()
            if ack != DrainAck(prior.generation, 0, True):
                raise Refused('drain_ack')
            a.restore(target)
            if fail_health:
                a.change(healthy=False)
            if not a.verify(target):
                raise Refused('apply_health')
            record['phase'] = 'applied'
            atomic(a.root / 'journal', record)
            a.release()
            return record
        except Exception:
            restore_locked(a, record)
            raise Refused('apply_failed_rolled_back') from None


class FixtureExecutor:
    def __init__(self, root):
        self.root = FilesystemFixture(root).root.resolve()

    def apply(self, job: Job, binding: FixtureBinding, *, cancel=lambda: False,
              accept=lambda: False, lease='local-fixture'):
        """Opt-in fixture only. Durable intent before independent supervision.

        accept is trusted confirmation of coordinator PREPARED, never terminal
        success. Returning applied does not release admission: WorkerHTTP must
        reconcile that durable local decision with the coordinator first.
        """
        job = validate_job(job, job.get('machine'))
        if job.action != 'apply' or job['state'] != 'running' or job.get('apply_phase'):
            raise Refused('invalid_apply_state')
        if type(binding) is not FixtureBinding or binding.apply_enabled is not True:
            raise Refused('unsupported_adapter')
        deadline = timestamp(job.apply_request.deadline_at)
        if cancel() or deadline <= time.time():
            raise Refused('cancelled_or_expired')
        a = FilesystemFixture(self.root)
        with exclusive(self.root):
            previous = load(self.root / 'journal')
            if previous['phase'] not in ('idle', 'restored', 'applied', 'rolled_back'):
                raise Refused('recovery_required')
            if previous.get('action') == 'apply' and not previous.get('agreement'):
                raise Refused('recovery_required')
            binding.check(job, a)
            seen_path = self.root / 'apply_ids'
            seen = load(seen_path) if seen_path.exists() else []
            if job['id'] in seen:
                raise Refused('replay')
            if len(seen) >= 256:
                raise Refused('capacity')
            prior = a.snapshot()
            # Configuration is built exclusively from typed recommendation fields.
            config = json.dumps({k: job['options'][k] for k in ('context', 'parallel')}, sort_keys=True)
            target = PriorState(digest((prior.generation + job['id'] + config).encode()), config, prior.service)
            record = dict(phase='apply_pending', action='apply', job_id=job['id'],
                          prior=asdict(prior), target=asdict(target), pgid=None,
                          restored=False, failure='execution', deadline=deadline,
                          fixture_mode=binding.mode, commit_nonce=os.urandom(32).hex(),
                          transaction_id=job.apply_request.transaction_id,
                          generation=job.apply_request.generation, machine=job['machine'],
                          lease=lease, job=dict(job))
            atomic(seen_path, seen + [job['id']])
            atomic(self.root / 'journal', record)
        p = subprocess.Popen([sys.executable, '-m', 'worker.benchmark_watchdog',
                              str(self.root), 'apply', job['id']],
                             stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, start_new_session=True,
                             cwd=Path(__file__).resolve().parent.parent,
                             env={'PATH': os.defpath, 'PYTHONUNBUFFERED': '1'})
        try:
            offered = False
            while p.poll() is None:
                if cancel() or time.time() >= deadline:
                    break
                ready = load(self.root / 'journal')
                if ready.get('phase') == 'prepared' and not offered:
                    offered = True
                    if not accept():
                        break
                    # Trusted callback attests durable coordinator preparation.
                    atomic(self.root / 'commit_ack', dict(job_id=job['id'],
                           commit_nonce=record['commit_nonce']))
                time.sleep(.02)
        finally:
            p.stdin.close()  # EOF stops execution; prepared remains recoverable
            p.wait(timeout=6)
        record = load(self.root / 'journal')
        if p.returncode != 0 or record.get('job_id') != job['id'] or record['phase'] not in ('applied', 'rolled_back'):
            raise Refused('recovery_required')
        return record

    def run(self, *, budget_seconds=2.0, cancel=lambda: False, mode='complete',
            job_id='local-fixture', deadline_at=None):
        if mode not in ('complete', 'wait', 'fail') or not 0 < budget_seconds <= 600:
            raise Refused('execution_options')
        deadline = time.time() + budget_seconds
        if deadline_at is not None:
            deadline = min(deadline, deadline_at)
        # No input from a job can select executable, module, cwd or environment.
        p = subprocess.Popen([sys.executable, '-m', 'worker.benchmark_watchdog',
                              str(self.root), str(budget_seconds), mode, job_id, str(deadline)],
                             stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, start_new_session=True,
                             cwd=Path(__file__).resolve().parent.parent,
                             env={'PATH': os.defpath, 'PYTHONUNBUFFERED': '1'})
        try:
            end = time.monotonic() + budget_seconds + 5
            while p.poll() is None:
                if cancel() or time.monotonic() >= end:
                    p.stdin.close()  # EOF also covers executor SIGKILL
                    break
                time.sleep(.02)
            p.wait(timeout=6)
        finally:
            p.stdin.close()
        record = load(self.root / 'journal')
        if p.returncode != 0 or record.get('restored') is not True:
            raise Refused('recovery_required')
        return record


@dataclass(frozen=True)
class Assignment:
    job: Job
    lease: str
    recovery_only: bool
    action: str = ''


@dataclass(frozen=True)
class Update:
    sequence: int
    state: str
    progress: str
    failure: str = ''
    restored: bool = False
    summary: dict | None = None
    action: str = ''
    result: str = ''


@dataclass(frozen=True)
class Audit:
    actor: str
    code_revision: str
    config_generation: str


@dataclass(frozen=True)
class ApplyRequest:
    audit: Audit
    requested_at: str
    deadline_at: str
    completed_at: str
    completed_sequence: int
    transaction_id: str
    generation: int


class Job(dict):
    """Validated detached wire mapping, with typed additive Apply fields."""
    @property
    def action(self):
        return self.get('action', '')

    @property
    def result(self):
        return self.get('result', '')

    @property
    def apply_request(self):
        r = self.get('apply_request')
        return ApplyRequest(**(r | {'audit': Audit(**r['audit'])})) if r else None


def timestamp(value):
    if not isinstance(value, str) or not re.fullmatch(
            r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})', value):
        raise ValueError
    return datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()


def hex_value(value, lengths=(64,), nonzero=True):
    return (isinstance(value, str) and len(value) in lengths and
            re.fullmatch('[0-9a-f]+', value) is not None and
            (not nonzero or set(value) != {'0'}))


def validate_audit(value, required=False):
    if not isinstance(value, dict) or set(value) - {'actor', 'code_revision', 'config_generation'}:
        raise ValueError
    for k, v in value.items():
        if k == 'actor':
            if not isinstance(v, str) or not re.fullmatch('[a-z0-9][a-z0-9-]{0,31}', v):
                raise ValueError
        elif not hex_value(v, (40, 64) if k == 'code_revision' else (64,), k != 'code_revision'):
            raise ValueError
    if required and len(value) != 3:
        raise ValueError


def validate_apply_fields(job):
    action = job.get('action', '')
    if action not in ('', 'apply') or ('action' in job and not action):
        raise ValueError
    if not action:
        if 'result' in job or 'apply_request' in job or 'apply_phase' in job:
            raise ValueError
        return
    r = job['apply_request']
    if not isinstance(r, dict) or set(r) != {'audit', 'requested_at', 'deadline_at', 'completed_at', 'completed_sequence', 'transaction_id', 'generation'}:
        raise ValueError
    if not hex_value(r['transaction_id']) or type(r['generation']) is not int or r['generation'] != 1:
        raise ValueError
    phase = job.get('apply_phase', '')
    if ('apply_phase' in job and phase not in ('prepared', 'applied', 'rolled_back') or
            phase == 'prepared' and (job['state'] != 'running' or job['progress'] != 'validating' or
                                     not 1 <= job['sequence'] < 10000 or job.get('failure')) or
            phase in ('applied', 'rolled_back') and job.get('result') != phase):
        raise ValueError
    validate_audit(r['audit'], True)
    validate_audit({k: job['provenance'][k] for k in ('actor', 'code_revision', 'config_generation')}, True)
    if any(r['audit'][k] != job['provenance'][k] for k in ('code_revision', 'config_generation')):
        raise ValueError
    if (type(r['completed_sequence']) is not int or not 1 <= r['completed_sequence'] <= 10000 or
            timestamp(r['completed_at']) > timestamp(r['requested_at']) or
            timestamp(r['deadline_at']) - timestamp(r['requested_at']) != job['options']['budget_seconds']):
        raise ValueError
    if (phase == 'applied' and job['sequence'] < 2 or
            phase == 'rolled_back' and job['state'] != 'failed'):
        raise ValueError
    result = job.get('result', '')
    if 'result' in job and result not in ('applied', 'rolled_back'):
        raise ValueError
    terminal = job['state'] in ('completed', 'failed', 'cancelled')
    if terminal:
        if phase != result:
            raise ValueError
        if (job.get('progress') != 'done' or job['sequence'] < 1 or
                (job['state'] == 'failed' and not job.get('failure')) or
                (job['state'] == 'cancelled' and job.get('failure', '') != '') or
                (result == 'applied' and (job['state'] != 'completed' or job.get('restored') is not False or job.get('failure', '') != '')) or
                (result == 'rolled_back' and (job['state'] not in ('failed', 'cancelled') or job.get('restored') is not True)) or not result):
            raise ValueError
    elif result or job.get('restored', False):
        raise ValueError


def validate_job(job, worker):
    try:
        o, p = job['options'], job['provenance']
        if (not {'id', 'machine', 'options', 'provenance', 'state', 'progress', 'sequence', 'failure', 'restored', 'updated_at'} <= set(job) or
                not {'schema_version', 'policy_version', 'fixture_version', 'worker_id', 'created_at', 'deadline_at', 'model_sha256', 'build_digest'} <= set(p)):
            raise ValueError
        if (set(o) != {'context', 'parallel', 'budget_seconds', 'model_sha256', 'build_digest'} or
                set(p) - {'schema_version', 'policy_version', 'fixture_version', 'worker_id',
                          'created_at', 'deadline_at', 'model_sha256', 'build_digest',
                          'actor', 'code_revision', 'config_generation'} or
                set(job) - {'id', 'machine', 'options', 'provenance', 'state', 'progress',
                            'sequence', 'failure', 'restored', 'summary', 'updated_at', 'finished_at',
                            'action', 'result', 'apply_request', 'apply_phase'} or
                any(type(o[k]) is not int for k in ('context', 'parallel', 'budget_seconds')) or
                any(type(p[k]) is not int for k in ('schema_version', 'policy_version', 'fixture_version')) or
                not isinstance(job['id'], str) or not 1 <= len(job['id']) <= 128 or
                job['state'] not in {'queued', 'running', 'cancelling', 'completed', 'failed', 'cancelled', 'recovery_required'}):
            raise ValueError
        if (job['machine'] != worker or p['worker_id'] != worker or
                any(p[k] != 1 for k in ('schema_version', 'policy_version', 'fixture_version')) or
                any(o[k] != p[k] for k in ('model_sha256', 'build_digest')) or
                not 8192 <= o['context'] <= 131072 or o['context'] % 1024 or
                not 1 <= o['parallel'] <= 8 or not 30 <= o['budget_seconds'] <= 600 or
                type(job['sequence']) is not int or not 0 <= job['sequence'] <= 10000 or
                (job['sequence'] == 10000 and job['state'] not in ('completed', 'failed', 'cancelled'))):
            raise ValueError
        for k in ('model_sha256', 'build_digest'):
            v = o[k]
            if len(v) != 64 or any(c not in '0123456789abcdef' for c in v) or v == '0' * 64:
                raise ValueError
        timestamp(p['deadline_at'])
        for key in ('created_at',):
            if key in p:
                timestamp(p[key])
        for key in ('updated_at', 'finished_at'):
            if key in job:
                timestamp(job[key])
        validate_audit({k: p[k] for k in ('actor', 'code_revision', 'config_generation') if k in p})
        if ('restored' in job and type(job['restored']) is not bool or
                job.get('progress', 'preparing') not in {'pending', 'preparing', 'loading', 'warming', 'measuring', 'validating', 'restoring', 'done'} or
                job.get('failure', '') not in {'', 'deadline', 'restart', 'execution', 'quality', 'model_mismatch', 'restoration'}):
            raise ValueError
        if 'summary' in job:
            validate_summary(job['summary'], o)
        if job.get('action') == 'apply' and 'summary' not in job:
            raise ValueError
        validate_apply_fields(job)
        return Job(json.loads(json.dumps(job)))
    except (KeyError, TypeError, ValueError, AttributeError):
        raise Refused('invalid_job') from None


class WorkerHTTP:
    """No automatic mutation retries: reconcile ambiguous responses via status.

    URL/credential are trusted operator configuration; never taken from a job.
    No proxy environment, redirects, raw error bodies or lease logging.
    """
    def __init__(self, base_url, worker_id, token, *, transport=None):
        self.worker_id = worker_id
        self.client = httpx.Client(base_url=base_url, headers={'Authorization': 'Bearer ' + token},
                                   trust_env=False, follow_redirects=False, timeout=2,
                                   transport=transport)

    def close(self):
        self.client.close()

    def _post(self, action, **body):
        try:
            with self.client.stream('POST', '/worker/benchmark/' + action,
                                    json=dict(worker_id=self.worker_id, **body)) as r:
                if r.status_code != 200:
                    raise Refused('http_refused')
                raw = bytearray()
                for chunk in r.iter_bytes():
                    raw.extend(chunk)
                    if len(raw) > 16384:
                        raise Refused('response_limit')
                return strict(bytes(raw))
        except httpx.HTTPError:
            raise Refused('http_unavailable') from None

    def lease(self):
        value = self._post('lease', apply_protocol=1)
        if value == {'job': None}:
            return None
        if (not isinstance(value, dict) or set(value) - {'job', 'lease', 'recovery_only', 'action'} or
                not {'job', 'lease', 'recovery_only'} <= set(value) or
                type(value['recovery_only']) is not bool or not isinstance(value['lease'], str) or
                not value['lease']):
            raise Refused('invalid_assignment')
        job = validate_job(value['job'], self.worker_id)
        if value.get('action', '') != job.action or ('action' in value and value['action'] != 'apply'):
            raise Refused('invalid_assignment_action')
        if ((value['recovery_only'] and job['state'] not in {'cancelling', 'recovery_required'} and job.get('apply_phase') != 'prepared') or
                (not value['recovery_only'] and job['state'] != 'running')):
            raise Refused('invalid_assignment_state')
        return Assignment(job, value['lease'], value['recovery_only'], job.action)

    def status(self, job_id):
        job = validate_job(self._post('status', job_id=job_id), self.worker_id)
        if job['id'] != job_id:
            raise Refused('job_mismatch')
        return job

    def update(self, assignment, update: Update):
        if (type(update.sequence) is not int or not 1 <= update.sequence <= 10000 or
                type(update.restored) is not bool or
                update.state not in {'running', 'completed', 'failed', 'cancelled', 'recovery_required'} or
                update.progress not in {'preparing', 'loading', 'warming', 'measuring', 'validating', 'restoring', 'done'} or
                update.failure not in {'', 'deadline', 'restart', 'execution', 'quality', 'model_mismatch', 'restoration'}):
            raise Refused('invalid_update')
        validate_update(assignment, update)
        body = asdict(update)
        for key in ('action', 'result'):
            if not body[key]:
                del body[key]
        if body['summary'] is None:
            del body['summary']
        else:
            try:
                validate_summary(body['summary'], assignment.job['options'])
            except (ValueError, TypeError, KeyError):
                raise Refused('invalid_update_summary') from None
        job = validate_job(self._post('update', job_id=assignment.job['id'],
                                      lease=assignment.lease, update=body), self.worker_id)
        if job['id'] != assignment.job['id']:
            raise Refused('job_mismatch')
        return job

    def poll_fixture(self, executor: FixtureExecutor, binding: FixtureBinding | None = None):
        """One bounded polling iteration. Fixture NEVER reports inference success."""
        local = load(executor.root / 'journal')
        if local.get('action') == 'apply' and (not local.get('agreement') or
                FilesystemFixture(executor.root).state()['fenced']):
            return self._recover_apply(executor, local)
        assignment = self.lease()
        if assignment is None:
            return None
        j = assignment.job
        if assignment.action == 'apply':
            return self._poll_apply(executor, assignment, binding)
        failure = 'execution'
        try:
            if assignment.recovery_only:
                record = load(executor.root / 'journal')
                if record.get('job_id') != j['id']:
                    raise Refused('recovery_job_mismatch')
                result = recover(executor.root)
                failure = 'restart'
            else:
                if binding is not None:
                    binding.check(j, FilesystemFixture(executor.root), apply=False)
                deadline = datetime.fromisoformat(j['provenance']['deadline_at'].replace('Z', '+00:00')).timestamp()
                remaining = min(j['options']['budget_seconds'], deadline - time.time())
                if remaining <= 0:
                    raise Refused('expired')
                result = executor.run(budget_seconds=remaining, job_id=j['id'], deadline_at=deadline,
                    cancel=lambda: self.status(j['id'])['state'] != 'running')
                failure = result.get('failure', 'execution') or 'execution'
            restored = result.get('restored') is True
        except Exception:
            restored = False
        current = self.status(j['id'])
        if current['state'] in ('completed', 'failed', 'cancelled'):
            return current
        state = ('cancelled' if current['state'] == 'cancelling' else 'failed') if restored else 'recovery_required'
        reason = '' if state == 'cancelled' else (failure if restored else 'restoration')
        return self.update(Assignment(current, assignment.lease, assignment.recovery_only),
            Update(current['sequence'] + 1, state,
            'done' if restored else 'restoring', reason, restored))


    def reconcile(self, assignment, phase):
        if phase not in ('prepared', 'applied', 'rolled_back') or assignment.action != 'apply':
            raise Refused('invalid_reconciliation')
        r = assignment.job.apply_request
        response = validate_job(self._post('reconcile', job_id=assignment.job['id'],
            lease=assignment.lease, reconcile=dict(transaction_id=r.transaction_id,
            generation=r.generation, phase=phase, durable=True)), self.worker_id)
        self._matching_apply(assignment.job, response)
        if response.get('apply_phase') != phase:
            raise Refused('reconciliation_unconfirmed')
        return response

    def _matching_apply(self, pinned, current):
        if any(current.get(k) != pinned.get(k) for k in
               ('id', 'machine', 'action', 'apply_request', 'options', 'provenance', 'summary')):
            raise Refused('recovery_required')

    def _recover_apply(self, executor, record):
        a = FilesystemFixture(executor.root)
        with exclusive(a.root):
            a.fence()
            record = load(a.root / 'journal')
            try:
                pinned = validate_job(record['job'], self.worker_id)
                r = pinned.apply_request
                if (record['job_id'] != pinned['id'] or record['machine'] != self.worker_id or
                        record['transaction_id'] != r.transaction_id or record['generation'] != r.generation or
                        not isinstance(record['lease'], str) or not record['lease']):
                    raise Refused('recovery_required')
                assignment = Assignment(pinned, record['lease'], True, 'apply')
                current = self.status(pinned['id'])
                self._matching_apply(pinned, current)
                # Preserve the historical completed-restoration journal API.
                # The immutable decision remains explicit after agreement.
                if record['phase'] == 'restored' and record.get('decision') == 'rolled_back':
                    record['phase'] = 'rolled_back'
                def rollback():
                    try:
                        return restore_locked(a, record)
                    except Refused:
                        if not current.get('apply_phase') and current['state'] not in ('completed', 'failed', 'cancelled'):
                            self.update(Assignment(current, record['lease'], True, 'apply'),
                                Update(current['sequence'] + 1, 'recovery_required', 'restoring',
                                       'restoration', action='apply'))
                        raise

                phase = record['phase']
                if phase not in ('applied', 'rolled_back'):
                    if current.get('result'):
                        raise Refused('recovery_required')
                    if phase == 'prepared' and current.get('apply_phase') == 'prepared':
                        if not a.verify(PriorState(**record['target'])):
                            record = rollback()
                        else:
                            record['phase'] = 'applied'
                            atomic(a.root / 'journal', record)
                    else:
                        record = rollback()
                phase = record['phase']
                expected = PriorState(**record['target' if phase == 'applied' else 'prior'])
                if not a.verify(expected):
                    raise Refused('recovery_required')
                # Re-establish durability even if an earlier atomic replacement
                # became visible before its directory fsync failed.
                atomic(a.root / 'journal', record)
                if current.get('apply_phase') != phase:
                    # A lost response is resolved by authenticated status; otherwise
                    # retain the journal and fence for the next bounded retry.
                    try:
                        current = self.reconcile(assignment, phase)
                    except Refused:
                        current = self.status(pinned['id'])
                self._matching_apply(pinned, current)
                if current.get('apply_phase') != phase or current.get('result') != phase:
                    raise Refused('recovery_required')
                if not a.verify(expected):
                    raise Refused('recovery_required')
                record['agreement'] = True
                record['decision'] = phase
                if phase == 'rolled_back':
                    record['phase'] = 'restored'
                atomic(a.root / 'journal', record)
                a.release()
                return current
            except (KeyError, TypeError, ValueError):
                raise Refused('recovery_required') from None

    def _poll_apply(self, executor, assignment, binding):
        j = assignment.job

        def accept():
            try:
                current = self.reconcile(assignment, 'prepared')
            except Refused:
                current = self.status(j['id'])
            self._matching_apply(j, current)
            return current.get('apply_phase') == 'prepared'

        if not assignment.recovery_only:
            try:
                executor.apply(j, binding, accept=accept, lease=assignment.lease,
                    cancel=lambda: self.status(j['id'])['state'] != 'running')
            except Exception:
                pass  # watchdog owns mutation; never infer physical success
        record = load(executor.root / 'journal')
        if record.get('job_id') != j['id'] or record.get('action') != 'apply':
            if assignment.recovery_only:
                FilesystemFixture(executor.root).fence()
            current = self.status(j['id'])
            self._matching_apply(j, current)
            if current.get('apply_phase') or current.get('result'):
                raise Refused('recovery_required')
            return self.update(Assignment(current, assignment.lease, True, 'apply'),
                Update(current['sequence'] + 1, 'recovery_required', 'restoring',
                       'restoration', action='apply'))
        return self._recover_apply(executor, record)


@dataclass(frozen=True)
class FixtureBinding:
    """Trusted code options, NOT decoded from HTTP/browser inputs.

    Bytes are actual disposable fixture artifacts. No production adapter, dynamic
    import, command, path or executable can be chosen through this binding.
    """
    model: bytes
    build: bytes
    code_revision: str
    config_generation: str
    apply_enabled: bool = False
    mode: str = 'complete'

    def check(self, job, adapter, *, apply=True):
        if (type(self.model) is not bytes or type(self.build) is not bytes or
                self.mode not in ('complete', 'fail', 'wait') or
                not hex_value(self.code_revision, (40, 64), False) or
                not hex_value(self.config_generation) or
                digest(self.model) != job['options']['model_sha256'] or
                digest(self.build) != job['options']['build_digest'] or
                adapter.snapshot().generation != self.config_generation):
            raise Refused('stale_binding')
        if not apply and any(job['provenance'].get(k) != getattr(self, k)
                             for k in ('code_revision', 'config_generation')):
            raise Refused('stale_binding')
        audit = job.apply_request.audit if apply else None
        if apply and (audit.code_revision != self.code_revision or
                      audit.config_generation != self.config_generation):
            raise Refused('stale_binding')


def validate_summary(s, o):
    required = {'quality', 'context', 'parallel', 'prompt_tokens', 'output_tokens',
                'duration_seconds', 'tokens_per_second', 'model_verified', 'model_sha256', 'build_digest'}
    if not isinstance(s, dict) or not required <= set(s) or set(s) - required - {'output_allowance', 'reserved_tokens'}:
        raise ValueError
    if (s['quality'] != 'pass' or s['model_verified'] is not True or
            any(s[k] != o[k] for k in ('context', 'parallel', 'model_sha256', 'build_digest')) or
            any(type(s[k]) is not int for k in ('context', 'parallel', 'prompt_tokens', 'output_tokens')) or
            not 1 <= s['prompt_tokens'] < o['context'] or not 1 <= s['output_tokens'] <= o['context']):
        raise ValueError
    allowance, reserve = s.get('output_allowance', 0), s.get('reserved_tokens', 0)
    if type(allowance) is not int or type(reserve) is not int:
        raise ValueError
    if allowance == reserve == 0:
        if s['prompt_tokens'] + s['output_tokens'] != o['context']:
            raise ValueError
    else:
        remaining = o['context'] - s['prompt_tokens'] - allowance
        if not (s['output_tokens'] <= allowance <= o['context'] and 0 <= reserve <= 104 and
                reserve <= remaining <= 104 and remaining - reserve <= 96):
            raise ValueError
    duration, tps = s['duration_seconds'], s['tokens_per_second']
    if (type(duration) not in (int, float) or type(tps) not in (int, float) or
            not math.isfinite(duration) or not math.isfinite(tps) or
            not 0 < duration <= o['budget_seconds'] or not 0 < tps <= 1e6):
        raise ValueError
    want = s['output_tokens'] / duration
    if not 0 < want <= 1e6 or abs(tps - want) > want * .01:
        raise ValueError


def validate_update(assignment, u):
    if (u.sequence == 10000 and u.state not in ('completed', 'failed', 'cancelled') or
            u.sequence != assignment.job['sequence'] + 1 or
            assignment.job['state'] in ('completed', 'failed', 'cancelled')):
        raise Refused('invalid_update_sequence')
    if u.action != assignment.action or u.action not in ('', 'apply'):
        raise Refused('invalid_update_action')
    if not u.action:
        if u.result:
            raise Refused('invalid_update_result')
        return
    if u.state in ('completed', 'failed', 'cancelled') or assignment.job.get('apply_phase'):
        raise Refused('reconciliation_required')
    if u.summary is not None:
        raise Refused('invalid_update_summary')
    terminal = u.state in ('completed', 'failed', 'cancelled')
    if terminal:
        valid = u.progress == 'done' and (
            (u.result == 'applied' and u.state == 'completed' and not u.restored and not u.failure and not assignment.recovery_only and assignment.job['state'] == 'running') or
            (u.result == 'rolled_back' and u.restored and
             ((u.state == 'failed' and bool(u.failure)) or (u.state == 'cancelled' and not u.failure and assignment.job['state'] in ('cancelling', 'recovery_required')))))
    else:
        valid = not u.result and not u.restored and (
            (u.state == 'recovery_required' and u.progress == 'restoring' and u.failure == 'restoration') or
            (u.state == 'running' and assignment.job['state'] == 'running' and not u.failure and
             u.progress in ('preparing', 'loading', 'warming', 'measuring', 'validating', 'restoring')))
    if not valid:
        raise Refused('invalid_update_result')


def measurement_summary(artifact, profile, options, *, model_bytes, build_bytes, output_allowance):
    """Convert actual Python artifact rows; caller attests bytes are those loaded.

    No version string substitution, token padding or aggregate throughput. This
    function cannot itself establish loaded-file identity; production binding is
    deliberately unavailable. Discovery snapshots must be revalidated at load.
    """
    try:
        if (artifact['status'] != 'completed' or not artifact['selected'] or
                artifact['cleanup']['owned_processes_stopped'] is not True or
                artifact['cleanup']['owned_temp_removed'] is not True or
                artifact['cleanup']['final_safety_verified'] is not True or
                profile['passed'] is not True or
                set(profile['quality']) != {'caller_recipient_direction', 'grounded_retrieval',
                                            'instruction_following', 'structured_json'} or
                not all(v is True for v in profile['quality'].values()) or
                digest(model_bytes) != options['model_sha256'] or
                digest(build_bytes) != options['build_digest'] or
                any(profile[k] != options[k] for k in ('context', 'parallel'))):
            raise ValueError
        selected = next(c for c in artifact['candidates'] if c['id'] == artifact['selected'])
        if selected['status'] != 'passed' or profile not in selected['profiles']:
            raise ValueError
        row = next(r for r in profile['measurements'] if r['length'] == 'long' and r['warmup'] is False)
        prompt, output = row['prompt_tokens'], row['decode_tokens']
        duration, tps = row['decode_s'], row['decode_tps']
        slack = options['context'] - prompt - output_allowance - 8
        if (any(type(v) is not int for v in (prompt, output, output_allowance)) or
                not 0 <= slack <= 96 or not 1 <= output <= output_allowance or prompt < 1 or
                not math.isfinite(duration) or not 0 < duration <= options['budget_seconds'] or
                not math.isfinite(tps) or not 0 < tps <= 1e6 or
                not 0 < output / duration <= 1e6 or abs(tps - output / duration) > output / duration * .01):
            raise ValueError
        return dict(quality='pass', context=options['context'], parallel=options['parallel'],
                    prompt_tokens=prompt, output_tokens=output, output_allowance=output_allowance,
                    reserved_tokens=8, duration_seconds=duration, tokens_per_second=tps,
                    model_verified=True, model_sha256=digest(model_bytes), build_digest=digest(build_bytes))
    except (KeyError, TypeError, ValueError, StopIteration, ZeroDivisionError):
        raise Refused('invalid_evidence') from None
