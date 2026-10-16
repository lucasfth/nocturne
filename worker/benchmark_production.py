# SPDX-License-Identifier: AGPL-3.0-or-later
"""Installed-only production runner. No fixture or job-selected paths.

Execution requires the root service's explicit installed capability attestation;
source availability alone is not installation attestation. Every existing journal
is reconciled against coordinator status and a matching local restoration receipt
before leasing; missing status or evidence remains an operator-recovery block.
Replay never reexecutes inference or releases the service fence.
"""
from __future__ import annotations

import asyncio
import contextlib
from dataclasses import asdict
import json
import os
import select
from pathlib import Path
import signal
import subprocess
import sys
import time

from . import benchmark_artifacts as artifacts
from . import benchmark_discovery as discovery
from . import model_benchmark as benchmark
from .benchmark_service import ServiceClient, ROOT, protected
from .benchmark_executor import (Assignment, Refused, Update, WorkerHTTP,
                                 timestamp, validate_job, validate_summary,
                                 atomic, load, private_root, exclusive, hex_value)

PYTHON = Path('/opt/nocturne/.venv/bin/python')
MODULE = 'worker.benchmark_production'
STATE = Path('/opt/nocturne/data/benchmark-production')


def journal_root():
    private_root(STATE.parent)
    STATE.mkdir(mode=0o700, exist_ok=True)
    root = private_root(STATE)
    fd = os.open(STATE.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    return root


def binding(job):
    return {k: job[k] for k in ('id', 'machine', 'options', 'provenance')}


def profile_config(config, options):
    context, parallel = options.get('context', 32768), options.get('parallel', 2)
    if (type(context) is not int or not 8192 <= context <= 32768 or context % 1024 or
            type(parallel) is not int or not 1 <= parallel <= 2):
        raise Refused('unsupported_profile')
    # Native model eligibility remains >=32768 in InstalledArtifacts. Context is
    # per slot; Backend multiplies by parallel and exercise fills each slot.
    config['min_context'] = context
    config['profiles'] = [dict(context=context, parallel=parallel, spec_n=2)]


def replay(job, record, service):
    if record.get('binding') != binding(job):
        raise Refused('journal_binding')
    result = record.get('result', {})
    # A fresh health check is never evidence of restoration. Require a durable
    # receipt from root verify of the pinned prior, plus an unchanged snapshot.
    if (result.get('restored') is True and record.get('verified_prior') == record.get('prior')
            and record.get('prior') is not None
            and asdict(service.snapshot()) == record['prior']):
        return result
    return {'restored': False, 'summary': None}


def installation_present():
    # A broken/partial install must not silently recover legacy admission.
    return os.path.lexists(ROOT)


def admission_allowed():
    return not installation_present() or not ServiceClient.admission_fenced()


def verified_source():
    if sys.platform != 'darwin' or os.geteuid() == 0:
        raise Refused('installed_unprivileged_required')
    if Path(__file__).absolute() != Path('/opt/nocturne/worker/benchmark_production.py'):
        raise Refused('installed_source_required')
    for name in ('__init__', 'benchmark_production', 'benchmark_service',
                 'benchmark_executor', 'benchmark_artifacts', 'benchmark_discovery',
                 'model_benchmark', 'macos_sensors'):
        protected(Path('/opt/nocturne/worker') / (name + '.py'))
    protected(PYTHON.resolve())


def capable(service):
    value = service.ping()
    return (value.get('service_control') is True and
            value.get('benchmark_execution') is True)


def installed_models(directory):
    # artifacts.inventory is for caller-owned discovery; installed inventory is
    # root-owned instead. Keep its flat/capped/all-or-nothing semantics.
    root = protected(directory, directory=True)
    paths, total = [], 0
    with os.scandir(root) as entries:
        for count, entry in enumerate(entries, 1):
            if count > artifacts.MAX_ENTRIES:
                raise Refused('inventory_limit')
            if entry.name.lower().endswith('.gguf'):
                path = protected(Path(root) / entry.name)
                total += path.stat().st_size
                paths.append(path)
                if len(paths) > artifacts.MAX_MODELS or total > artifacts.MAX_TOTAL_BYTES:
                    raise Refused('inventory_limit')
    models = discovery.discover(sorted(paths), output_reserve=256)
    if not models:
        raise Refused('empty_inventory')
    return sorted(models, key=lambda m: (-m.parameters, m.sha256, str(m.path)))


@contextlib.asynccontextmanager
async def open_artifacts(server, models):
    # Streaming model verification can take minutes. Keep the watchdog event
    # loop responsive to root keepalives and EOF; never close FDs underneath a
    # still-running verifier thread or proceed to spawn after cancellation.
    installed = artifacts.InstalledArtifacts(server, models)
    opening = asyncio.create_task(asyncio.to_thread(installed.__enter__))
    try:
        await asyncio.shield(opening)
    except asyncio.CancelledError:
        try:
            await opening
        except Exception:
            pass
        else:
            installed.__exit__(None, None, None)
        raise
    try:
        yield installed
    finally:
        installed.__exit__(None, None, None)


def measurement_summary(artifact, profile, options, *, model_digest, build_digest):
    """Same evidence contract as executor, with streaming-verified digests."""
    try:
        if (artifact['status'] != 'completed' or not artifact['selected'] or
                any(artifact['cleanup'].get(k) is not True for k in
                    ('owned_processes_stopped', 'owned_temp_removed', 'final_safety_verified')) or
                profile['passed'] is not True or
                set(profile['quality']) != {'caller_recipient_direction', 'grounded_retrieval',
                                           'instruction_following', 'structured_json'} or
                not all(v is True for v in profile['quality'].values()) or
                model_digest != options['model_sha256'] or build_digest != options['build_digest'] or
                any(profile[k] != options[k] for k in ('context', 'parallel'))):
            raise ValueError
        selected = next(c for c in artifact['candidates'] if c['id'] == artifact['selected'])
        if selected['status'] != 'passed' or profile not in selected['profiles']:
            raise ValueError
        row = next(r for r in profile['measurements'] if r['length'] == 'long' and r['warmup'] is False)
        result = dict(quality='pass', context=options['context'], parallel=options['parallel'],
                      prompt_tokens=row['prompt_tokens'], output_tokens=row['decode_tokens'],
                      output_allowance=256, reserved_tokens=8, duration_seconds=row['decode_s'],
                      tokens_per_second=row['decode_tps'], model_verified=True,
                      model_sha256=model_digest, build_digest=build_digest)
        validate_summary(result, options)
        return result
    except (KeyError, TypeError, ValueError, StopIteration):
        raise Refused('invalid_evidence') from None


class InstalledProbe:
    """Local-only policy, never constructed from job fields or persisted PIDs.

    Root attests the fixed launchd worker on every snapshot. The runner pins its
    own identity locally; root does not need to trust a caller-supplied PID.
    """
    def __init__(self, service):
        self.service = service
        self.runner_pid = os.getpid()
        self.runner_identity = None

    async def admission(self):
        try:
            value = await asyncio.to_thread(self.service.call, 'admission')
            if (type(value) is not dict or set(value) !=
                    {'worker_pid', 'worker_executable', 'worker_start'} or
                    type(value['worker_pid']) is not int or value['worker_pid'] <= 1 or
                    value['worker_pid'] == self.runner_pid):
                raise ValueError
            exe, start = value['worker_executable'], value['worker_start']
            if (type(exe) is not str or not exe.startswith('/') or
                    len(exe) > 4096 or '\n' in exe or '\x00' in exe or
                    type(start) is not str or len(start) > 128 or
                    len(start.split()) != 5):
                raise ValueError
            if os.getpid() != self.runner_pid:
                raise ValueError
            text = await benchmark.command('/bin/ps', '-p', str(self.runner_pid),
                                           '-o', 'pid=,lstart=,comm=')
            fields = text.strip().split(None, 6)
            if len(fields) != 7 or int(fields[0]) != self.runner_pid:
                raise ValueError
            identity = (fields[6], ' '.join(fields[1:6]))
            if self.runner_identity is None:
                self.runner_identity = identity
            if identity != self.runner_identity:
                raise ValueError
            return {value['worker_pid']: (exe, ' '.join(start.split())),
                    self.runner_pid: self.runner_identity}
        except Exception:
            raise benchmark.Refused('installed_admission') from None

    async def __call__(self, disk, owned_pid=None):
        return await benchmark.probe(disk, owned_pid, trusted_local_adapter=self)


async def transaction(job, service):
    """Independent runner owns Backend children and waits for teardown BEFORE restore."""
    path = journal_root() / 'journal.json'
    record = load(path)
    if record.get('binding') != binding(job) or record.get('started'):
        raise Refused('journal_binding')
    prior = await asyncio.to_thread(service.snapshot)
    # Installation generation is NOT the mutable supervisor digest. Capture
    # the latter while holding this same exclusive root lease, before benchmark.
    capabilities = await asyncio.to_thread(service.ping)
    if capabilities.get('production_apply') is True:
        catalog = await asyncio.to_thread(service.call, 'presets')
        record['supervisor_digest'] = catalog['config_generation']
    record.update(started=True, prior=asdict(prior))
    atomic(path, record)  # job/options/provenance/lease and prior BEFORE mutation
    summary = None
    restored = False
    try:
        inventory = await asyncio.to_thread(service.inventory)
        if (job.action or job['provenance'].get('config_generation') != prior.generation or
                job['provenance'].get('code_revision') != inventory.code_revision):
            raise Refused('stale_binding')
        models = await asyncio.to_thread(installed_models, inventory.model_directory)
        model = models[0]  # largest strict-quality candidate, no smaller fallback
        if model.sha256 != job['options']['model_sha256']:
            raise Refused('model_mismatch')
        async with open_artifacts(inventory.server, [model]) as installed:
            fd = artifacts._protected_open(inventory.server, executable=True)
            try:
                build = await asyncio.to_thread(artifacts.digest_fd, fd)
            finally:
                os.close(fd)
            if build != job['options']['build_digest']:
                raise Refused('build_mismatch')
            c = installed.config
            profile_config(c, job['options'])
            c['runtime_s'] = min(job['options']['budget_seconds'],
                                 timestamp(job['provenance']['deadline_at']) - time.time())
            if c['runtime_s'] <= 6:
                raise Refused('deadline')
            await asyncio.to_thread(service.fence)
            ack = await asyncio.to_thread(service.drain)
            if ack.generation != prior.generation or ack.active != 0 or not ack.service_stopped:
                raise Refused('drain_ack')
            report = benchmark.plan(c)
            with benchmark.Lock():
                await benchmark.execute(c, report, probe_fn=InstalledProbe(service),
                                        backend_factory=installed.backend_factory)
            installed.verify()
            profile = report['candidates'][0]['profiles'][0]
            summary = measurement_summary(report, profile, job['options'],
                                          model_digest=model.sha256, build_digest=build)
    except (Exception, asyncio.CancelledError):
        summary = None
    finally:
        # Root also restores on disconnect. Never claim success on EOF alone.
        try:
            await asyncio.to_thread(service.restore, prior)
            restored = await asyncio.to_thread(service.verify, prior)
            if restored:
                record['verified_prior'] = asdict(prior)
                record['result'] = dict(restored=True, summary=summary)
                atomic(path, record)  # fsync root verification receipt before release/report
                await asyncio.to_thread(service.release)
        except Exception:
            restored = False
    result = dict(restored=restored, summary=summary if restored else None)
    record['result'] = result
    atomic(path, record)
    return result


async def watchdog(job, service, parent_fd=0):
    """Parent pipe EOF/cancel/deadline interrupts real execution, not the daemon.

    Backend's cancellation-safe spawn and stop own and reap each dedicated group.
    No persisted PID is ever signalled. SIGKILL of this watchdog remains an
    operator-recovery case; root refuses restore while inference remains alive.
    """
    import threading
    lock = threading.Lock()

    class Serialized:
        def __getattr__(self, name):
            def call(*args):
                with lock:
                    return getattr(service, name)(*args)
            return call

    rpc = Serialized()
    task = asyncio.create_task(transaction(job, rpc))
    loop = asyncio.get_running_loop()
    stopping = False

    def stop():
        nonlocal stopping
        if not stopping:
            stopping = True
            task.cancel()

    def readable():
        if os.read(parent_fd, 1) == b'':
            loop.remove_reader(parent_fd)
            stop()

    loop.add_reader(parent_fd, readable)
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, stop)

    async def monitor():
        deadline = min(timestamp(job['provenance']['deadline_at']),
                       time.time() + job['options']['budget_seconds'])
        while not task.done():
            if time.time() >= deadline:
                stop()
                return
            try:
                await asyncio.to_thread(rpc.ping)
            except Exception:
                stop()
                return
            await asyncio.sleep(1)

    keeper = asyncio.create_task(monitor())
    try:
        return await task
    finally:
        loop.remove_reader(parent_fd)
        keeper.cancel()
        await asyncio.gather(keeper, return_exceptions=True)
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.remove_signal_handler(sig)


def launch(job, cancel):
    verified_source()
    # -I requires operator installation into this interpreter's site-packages;
    # cwd/PYTHONPATH/user site cannot supply executable code.
    p = subprocess.Popen([str(PYTHON), '-I', '-m', MODULE], cwd='/',
                         env={'PATH': '/usr/bin:/bin', 'PYTHONUNBUFFERED': '1'},
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                         stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        p.stdin.write(json.dumps(dict(job), separators=(',', ':')).encode() + b'\n')
        p.stdin.flush()
        while p.poll() is None:
            if cancel():
                break
            time.sleep(.5)
    finally:
        p.stdin.close()  # EOF on cancellation AND agent death
        # Do not kill watchdog: it owns teardown/restoration, independently.
        p.wait(timeout=130)
    try:
        raw = p.stdout.read(16385)
        if p.returncode or len(raw) > 16384:
            raise Refused('recovery_required')
        return json.loads(raw)
    finally:
        p.stdout.close()


def poll_once(http, stop):
    verified_source()
    with ServiceClient() as service:
        http.production_apply = False
        if not capable(service):
            return  # never lease on service-control-only installation
        http.production_apply = service.ping().get('production_apply') is True
    with exclusive(journal_root()):
        return poll_locked(http, stop)


def reconcile(http, path, record):
    """Status and durable local evidence, never a lease or another inference run.

    An update may have committed even when its response was lost. Leave the
    journal intact on every error and consult status again on the next poll.
    Terminal coordinator state alone is not permission to forget local recovery.
    """
    job = record['job']
    current = http.status(job['id'])
    if binding(current) != record['binding'] or binding(job) != record['binding']:
        raise Refused('journal_binding')
    if not record.get('lease'):
        raise Refused('journal_lease')
    if current.get('action') == 'apply':
        # Coordinator has advanced this same benchmark to Apply. Preserve its
        # completed receipt; only the Apply handshake may update it now.
        if not record.get('reported') or not record.get('completed'):
            raise Refused('missing_completed_receipt')
        return True
    with ServiceClient() as service:
        result = replay(current, record, service)
    restored = result.get('restored') is True
    if current['state'] in ('completed', 'failed', 'cancelled'):
        if not restored:
            raise Refused('unresolved_journal')
    else:
        report(http, Assignment(current, record['lease'], True), current, result)
        if not restored:
            return False
    completed = current if current['state'] in ('completed', 'failed', 'cancelled') else http.status(job['id'])
    if binding(completed) != record['binding']:
        raise Refused('journal_binding')
    if completed['state'] == 'completed':
        record['completed'] = dict(completed)
    record['reported'] = True
    atomic(path, record)
    return True


def report(http, assignment, current, result):
    restored = result.get('restored') is True
    summary = result.get('summary')
    state = ('cancelled' if current['state'] == 'cancelling' else
             'completed' if summary else 'failed') if restored else 'recovery_required'
    http.update(Assignment(current, assignment.lease, assignment.recovery_only),
                Update(current['sequence'] + 1, state, 'done' if restored else 'restoring',
                       '' if state in ('completed', 'cancelled') else
                       'execution' if restored else 'restoration', restored,
                       summary if state == 'completed' else None))


def apply_binding(job, record, service):
    """Bind a completed local benchmark to exact protected catalog bytes."""
    completed = record.get('completed', {})
    r = job.apply_request
    if (record.get('binding') != binding(job) or not record.get('reported') or
            completed.get('state') != 'completed' or
            completed.get('sequence') != r.completed_sequence or
            completed.get('finished_at') != r.completed_at or
            completed.get('summary') != job.get('summary') or
            record.get('result', {}).get('summary') != job.get('summary')):
        raise Refused('benchmark_receipt')
    validate_summary(job['summary'], job['options'])
    if replay(job, record, service).get('restored') is not True:
        raise Refused('benchmark_restoration')
    inventory = service.inventory()
    if (inventory.code_revision != r.audit.code_revision or
            record['prior']['generation'] != r.audit.config_generation):
        raise Refused('stale_installation')
    catalog = service.call('presets')
    digest = record.get('supervisor_digest')
    if not hex_value(digest) or catalog['config_generation'] != digest:
        raise Refused('stale_supervisor')
    return abort_binding(job, record, catalog)


def abort_binding(job, record, catalog):
    """Select immutable bindings, NOT evidence that rollback occurred.

    Stale evidence can only be resolved by root abort against the ORIGINAL
    digest. If root refuses, an operator must reconcile the root journal or
    restore the benchmark-bound original configuration/service health, then
    retry this same transaction. Never delete apply.json, replace its digest
    with current bytes, or manufacture a benchmark restoration receipt.
    """
    if record.get('binding') != binding(job) or not hex_value(record.get('supervisor_digest')):
        raise Refused('missing_abort_binding')
    o = job['options']
    expected = dict(model_sha256=o['model_sha256'], server_sha256=o['build_digest'],
                    ctx_size=o['context'] * o['parallel'], parallel=o['parallel'],
                    cache_type_k='q8_0', cache_type_v='q8_0')
    matches = [key for key, value in catalog['presets'].items() if value == expected]
    if len(matches) != 1 or not hex_value(matches[0]):
        raise Refused('protected_preset')
    return matches[0], record['supervisor_digest']


def root_decision(job, record, value):
    r = job.apply_request
    if (not isinstance(value, dict) or value.get('transaction_id') != r.transaction_id or
            type(value.get('generation')) is not int or value['generation'] != r.generation or
            value.get('preset') != record.get('preset') or
            value.get('original_config_digest') != record.get('supervisor_digest') or
            value.get('phase') not in ('prepared', 'mutating', 'rolling_back', 'rollback_restoring', 'applied', 'rolled_back')):
        raise Refused('root_decision_binding')
    return value['phase']


def emit(value):
    print(json.dumps(value, separators=(',', ':')), flush=True)


def pipe_line(fd, deadline, heartbeat=None):
    # Bytewise bounded reading avoids buffered read-ahead across handshake steps.
    raw = bytearray()
    while time.monotonic() < deadline:
        if select.select([fd], [], [], min(1, max(0, deadline-time.monotonic())))[0]:
            byte = os.read(fd, 1)
            if not byte or len(raw) >= 16384:
                raise Refused('parent_pipe')
            raw.extend(byte)
            if byte == b'\n':
                return json.loads(raw)
        elif heartbeat:
            heartbeat()
    raise Refused('apply_deadline')


def apply_child(job, service):
    """Only this fixed, reattested process calls mutating Apply RPCs.

    EOF leaves recovery to root, which independently proves runner absence.
    Never infer rollback from an exception or retry an ambiguous apply call.
    """
    path = journal_root() / 'apply.json'
    record = load(path)
    if record.get('job') != dict(job) or not record.get('lease'):
        raise Refused('apply_journal')
    deadline = time.monotonic() + 110  # bounded even when recovering expired jobs
    emit({'phase': 'ready'})
    command = pipe_line(0, deadline, service.ping)
    if command == {'command': 'start'} and not record.get('attempted'):
        if time.time() >= timestamp(job.apply_request.deadline_at):
            raise Refused('apply_deadline')
        benchmark_record = load(journal_root() / 'journal.json')
        try:
            preset, digest = apply_binding(job, benchmark_record, service)
        except Refused:
            # Binding refusal is not rollback evidence. Reconcile a no-op with
            # root using the original digest; a stale digest stays fenced.
            record['recovery_required'] = 'root_abort_original_binding'
            atomic(path, record)
            return recover_apply_child(job, service, path, record, deadline)
        record.update(preset=preset, supervisor_digest=digest, attempted=True)
        atomic(path, record)
        # replay above took the snapshot required by prepare on this connection.
        r = job.apply_request
        value = service.call('prepare', preset=preset, transaction_id=r.transaction_id,
                             generation=r.generation, config_generation=digest)
        phase = root_decision(job, record, value)
        if phase != 'prepared':
            raise Refused('prepare_decision')
        record['phase'] = phase
        atomic(path, record)
        emit({'phase': phase})
        agreement = pipe_line(0, deadline, service.ping)
        if agreement != {'agreement': 'prepared'}:
            raise Refused('coordinator_agreement')
        if time.time() >= timestamp(job.apply_request.deadline_at):
            value = service.call('rollback')
        else:
            value = service.call('apply')
    elif command == {'command': 'recover'}:
        return recover_apply_child(job, service, path, record, deadline)
    else:
        raise Refused('apply_command')
    return finish_apply_child(job, service, path, record, deadline, value)


def recover_apply_child(job, service, path, record, deadline):
    # Never prepare/apply here, including the attempted-before-send window.
    if not record.get('attempted'):
        try:
            preset, digest = abort_binding(job, load(journal_root() / 'journal.json'),
                                           service.call('presets'))
        except Refused:
            record['recovery_required'] = 'operator_restore_original_benchmark_binding'
            atomic(path, record)
            raise
        record.update(preset=preset, supervisor_digest=digest, attempted=True)
        atomic(path, record)
    if not hex_value(record.get('preset')) or not hex_value(record.get('supervisor_digest')):
        raise Refused('missing_abort_binding')
    value = service.call('status')
    r = job.apply_request
    if isinstance(value, dict) and value.get('transaction_id') == r.transaction_id:
        phase = root_decision(job, record, value)
        if phase not in ('applied', 'rolled_back'):
            value = service.call('recover')
    else:
        # Null/latest-other status alone proves nothing. Root rejects abort if
        # ANY active journal or a receipt for this transaction exists.
        record['recovery_required'] = 'root_abort_original_binding'
        atomic(path, record)
        value = service.call('abort', preset=record['preset'], transaction_id=r.transaction_id,
                             generation=r.generation,
                             config_generation=record['supervisor_digest'])
    return finish_apply_child(job, service, path, record, deadline, value)


def finish_apply_child(job, service, path, record, deadline, value):
    phase = root_decision(job, record, value)
    if phase not in ('applied', 'rolled_back'):
        raise Refused('nonterminal_recovery')
    record['phase'] = phase
    atomic(path, record)
    emit({'phase': phase})
    if pipe_line(0, deadline, service.ping) != {'agreement': phase}:
        raise Refused('coordinator_agreement')
    r = job.apply_request
    service.call('finalize', transaction_id=r.transaction_id, generation=r.generation,
                 agreement=phase)
    record['finalized'] = True
    atomic(path, record)
    emit({'phase': 'finalized'})
    return 0


def coordinator_agreement(http, assignment, phase):
    current = http.status(assignment.job['id'])
    http._matching_apply(assignment.job, current)
    if current.get('apply_phase') != phase:
        try:
            current = http.reconcile(assignment, phase)
        except Exception:
            current = http.status(assignment.job['id'])  # lost acknowledgement
    http._matching_apply(assignment.job, current)
    if (current.get('apply_phase') != phase or
            (phase != 'prepared' and current.get('result') != phase) or
            (phase == 'prepared' and current['state'] != 'running')):
        raise Refused('coordinator_agreement')


def launch_apply(http, record, stop, *, recovery):
    verified_source()
    job = validate_job(record['job'], record['job']['machine'])
    assignment = Assignment(job, record['lease'], recovery, 'apply')
    # Credentials stay in this parent, never argv, env, journal or child pipe.
    p = subprocess.Popen([str(PYTHON), '-I', '-m', MODULE], cwd='/',
                         env={'PATH': '/usr/bin:/bin', 'PYTHONUNBUFFERED': '1'},
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                         stderr=subprocess.DEVNULL, start_new_session=True)
    def send(value):
        p.stdin.write(json.dumps(value, separators=(',', ':')).encode() + b'\n')
        p.stdin.flush()
    def check():
        if stop.is_set():
            raise Refused('stopping')
    deadline = time.monotonic() + 115
    try:
        send(dict(job))
        if pipe_line(p.stdout.fileno(), deadline, check) != {'phase': 'ready'}:
            raise Refused('runner_protocol')
        # Recheck after child readiness, not merely before spawn: crashes can
        # leave a durable but never-attempted record for arbitrarily long.
        if not recovery:
            current = http.status(job['id'])
            http._matching_apply(job, current)
            recovery = (current['state'] != 'running' or bool(current.get('apply_phase')) or
                        bool(current.get('result')) or
                        time.time() >= timestamp(job.apply_request.deadline_at) or
                        bool(record.get('recovery_required')))
        check()
        send({'command': 'recover' if recovery else 'start'})
        while True:
            check()
            message = pipe_line(p.stdout.fileno(), deadline, check)
            if message == {'phase': 'finalized'}:
                break
            if type(message) is not dict or set(message) != {'phase'} or message['phase'] not in (
                    'prepared', 'applied', 'rolled_back'):
                raise Refused('runner_protocol')
            phase = message['phase']
            coordinator_agreement(http, assignment, phase)
            send({'agreement': phase})
    finally:
        p.stdin.close()
        # Never signal a persisted PID or kill a process during root mutation.
        try:
            p.wait(timeout=130)
        finally:
            p.stdout.close()


def poll_locked(http, stop):
    apply_path = journal_root() / 'apply.json'
    if apply_path.exists():
        pending = load(apply_path)
        if not pending.get('finalized'):
            launch_apply(http, pending, stop, recovery=bool(pending.get('attempted')))
            return
    path = journal_root() / 'journal.json'
    record = load(path) if path.exists() else None
    consumed = (apply_path.exists() and load(apply_path).get('finalized') is True and
                record and binding(load(apply_path)['job']) == record.get('binding'))
    if record and not consumed and not reconcile(http, path, record):
        return
    assignment = http.lease()
    if assignment is None:
        return
    job = assignment.job
    if assignment.action:
        if not getattr(http, 'production_apply', False):
            raise Refused('production_apply_disabled')
        pending = dict(job=dict(job), lease=assignment.lease, finalized=False)
        if assignment.recovery_only:
            raise Refused('missing_apply_journal')
        atomic(apply_path, pending)
        return launch_apply(http, pending, stop, recovery=False)
    path = journal_root() / 'journal.json'
    record = load(path) if path.exists() else None
    result = {'restored': False, 'summary': None}
    if record and record.get('binding') == binding(job):
        with ServiceClient() as service:
            result = replay(job, record, service)
    elif not assignment.recovery_only:
        if record and not record.get('reported'):
            raise Refused('unresolved_journal')
        atomic(path, dict(binding=binding(job), job=dict(job), lease=assignment.lease,
                          started=False))
        try:
            launch(job, lambda: stop.is_set() or
                   http.status(job['id'])['state'] != 'running')
            with ServiceClient() as service:
                result = replay(job, load(path), service)
        except Exception:
            pass
    record = load(path) if path.exists() else None
    if record and record.get('binding') == binding(job):
        reconcile(http, path, record)
    else:
        current = http.status(job['id'])
        if binding(current) != binding(job):
            raise Refused('journal_binding')
        if current['state'] not in ('completed', 'failed', 'cancelled'):
            report(http, assignment, current, result)


class BenchmarkHTTP(WorkerHTTP):
    """Capability is refreshed from root before each lease; default is deny."""
    production_apply = False

    def _post(self, action, **body):
        if action == 'lease':
            body.pop('apply_protocol', None)
            if self.production_apply is True:
                body['apply_protocol'] = 1
        return super()._post(action, **body)


async def polling(url, worker_id, token):
    import threading
    stop = threading.Event()

    def run():
        http = BenchmarkHTTP(url, worker_id, token)
        try:
            while not stop.is_set():
                try:
                    poll_once(http, stop)
                except Exception:
                    pass  # static failure; never log credentials or remote bodies
                stop.wait(2)
        finally:
            http.close()

    task = asyncio.create_task(asyncio.to_thread(run))
    try:
        await asyncio.shield(task)
    finally:
        stop.set()
        await task


def main():
    try:
        verified_source()
        raw = sys.stdin.buffer.readline(16385)
        if len(raw) > 16384 or not raw.endswith(b'\n'):
            raise Refused('job_limit')
        obj = json.loads(raw)
        job = validate_job(obj, obj.get('machine'))
        with ServiceClient() as service:
            if not capable(service):
                raise Refused('execution_not_installed')
            if job.action:
                if service.ping().get('production_apply') is not True:
                    raise Refused('production_apply_disabled')
                return apply_child(job, service)
            result = asyncio.run(watchdog(job, service))
        print(json.dumps(result, separators=(',', ':')))
        return 0
    except Exception:
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
