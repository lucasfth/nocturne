# SPDX-License-Identifier: AGPL-3.0-or-later
"""Independent disposable-fixture supervisor. Not a production service manager.

Owns/reaps the child and process group, holds the transaction lock, watches stdin
EOF (parent death/cancel) and a monotonic deadline, and restores without network.
A watchdog SIGKILL during spawn deliberately leaves launch_pending fenced rather
than guessing process identity. Fixed child code cannot fork or run inference.
"""
from dataclasses import asdict
import os
import select
import signal
import subprocess
import sys
import time

from worker.benchmark_executor import (
    FilesystemFixture, DrainAck, PriorState, Refused, atomic, exclusive, load,
    group_absent, restore_locked,
)

# These are code constants, not shell fragments received from the coordinator.
CHILDREN = {
    'complete': 'import time; time.sleep(0.08)',
    'wait': 'import time; time.sleep(900)',
    'fail': 'raise SystemExit(7)',
}


def supervise(root, budget, mode, job_id='local-fixture', deadline_at=None):
    if mode not in CHILDREN or not 0 < budget <= 600:
        raise Refused('execution_options')
    a = FilesystemFixture(root)
    with exclusive(a.root):
        previous = load(a.root / 'journal')
        if (previous['phase'] not in ('idle', 'restored', 'applied', 'rolled_back') or
                previous.get('action') == 'apply' and not previous.get('agreement')):
            raise Refused('recovery_required')
        prior = a.snapshot()
        record = dict(phase='preparing', prior=asdict(prior), pgid=None,
                      restored=False, failure='execution', job_id=job_id)
        # Write-ahead record precedes even the local fence: admission also checks
        # journal phase under this same lock, including after a partial write.
        atomic(a.root / 'journal', record)
        child = None
        remaining = budget if deadline_at is None else min(budget, deadline_at - time.time())
        end = time.monotonic() + max(0, remaining)
        try:
            a.fence()
            if a.drain() != DrainAck(prior.generation, 0, True):
                raise Refused('drain_ack')
            if time.monotonic() >= end:
                record['failure'] = 'deadline'
                return
            record.update(phase='launching', launch_pending=True)
            atomic(a.root / 'journal', record)
            child = subprocess.Popen([sys.executable, '-I', '-c', CHILDREN[mode]],
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL, start_new_session=True,
                env={'PATH': os.defpath}, cwd=a.root)
            record.update(phase='running', pgid=child.pid, launch_pending=False)
            atomic(a.root / 'journal', record)
            while child.poll() is None:
                if time.monotonic() >= end:
                    record['failure'] = 'deadline'
                    break
                readable, _, _ = select.select([sys.stdin], [], [], .02)
                if readable and not os.read(sys.stdin.fileno(), 1):
                    break  # EOF on cancellation, lost parent, or lost connection
        finally:
            if child is not None:
                # Identity is safe here: the direct child has not been reaped
                # except by this supervisor. The fixed fixture cannot fork.
                if child.poll() is None:
                    os.killpg(child.pid, signal.SIGTERM)
                    try:
                        child.wait(timeout=.5)
                    except subprocess.TimeoutExpired:
                        os.killpg(child.pid, signal.SIGKILL)
                        child.wait(timeout=1)
                if not group_absent(child.pid):
                    raise Refused('processes_alive')
                record.update(pgid=None, launch_pending=False)
            atomic(a.root / 'journal', record)
            restore_locked(a, record)


def supervise_apply(root, job_id):
    """Fixed filesystem adapter only; no coordinator-selected callable or path.

    The independent watchdog owns mutation/rollback and watches parent EOF.
    SIGKILL of this watchdog leaves a durable snapshot and admission fence for
    restoration-only restart. Apply does not launch service processes.
    """
    a = FilesystemFixture(root)
    with exclusive(a.root):
        record = load(a.root / 'journal')
        if (record.get('phase') != 'apply_pending' or record.get('action') != 'apply' or
                record.get('job_id') != job_id):
            raise Refused('recovery_required')
        prior, target = PriorState(**record['prior']), PriorState(**record['target'])
        mode = record['fixture_mode']
        if mode not in CHILDREN:
            raise Refused('execution_options')
        end = time.monotonic() + min(600, max(0, record['deadline'] - time.time()))

        def revoked(delay=0):
            readable, _, _ = select.select([sys.stdin], [], [], delay)
            return (time.monotonic() >= end or
                    bool(readable and not os.read(sys.stdin.fileno(), 1)))

        committed = False
        mutated = False
        try:
            a.fence()
            if a.snapshot() != prior:
                raise Refused('stale_apply')
            # Drain changes service state too; rollback is required even if a
            # later candidate write fails partway through.
            mutated = True
            if a.drain() != DrainAck(prior.generation, 0, True):
                raise Refused('stale_apply')
            if revoked():
                return
            record['phase'] = 'applying'
            atomic(a.root / 'journal', record)
            a.restore(target)
            # Test-only health delay/failure, selected by trusted local code.
            if mode == 'wait':
                while not revoked(.02):
                    pass
                return
            if revoked(.1):
                return
            if mode == 'fail':
                a.change(healthy=False)
            if not a.verify(target):
                return
            # Durable local preparation precedes the coordinator attestation.
            record.update(phase='prepared')
            atomic(a.root / 'journal', record)
            # Private durable local acknowledgment; this process never uses HTTP.
            # Keep the lock and physical fence throughout coordinator arbitration.
            while time.monotonic() < end:
                ack_path = a.root / 'commit_ack'
                if ack_path.exists() and load(ack_path) == {
                        'job_id': job_id, 'commit_nonce': record['commit_nonce']}:
                    if not a.verify(target):
                        return
                    record['phase'] = 'applied'
                    atomic(a.root / 'journal', record)
                    # Irrevocable private commit, BEFORE terminal HTTP reconcile.
                    # Only matching coordinator terminal + health may unfence.
                    committed = True
                    return
                if revoked(.02):
                    return
        finally:
            if not committed:
                durable = load(a.root / 'journal')
                if not mutated and a.snapshot() != prior:
                    # No state belongs to this transaction. Keep the existing
                    # recovery interlock: both networkless recovery and HTTP
                    # reconciliation must refuse automatic snapshot restoration.
                    # Apply launches no child; launch_pending here deliberately
                    # represents unresolved ownership, not a PID to signal.
                    durable.update(phase='recovery_required', restored=False,
                                   launch_pending=True, mutation_started=False,
                                   recovery_reason='stale_apply')
                    atomic(a.root / 'journal', durable)
                    raise Refused('stale_apply')
                if durable.get('phase') not in ('prepared', 'applied', 'rolled_back'):
                    restore_locked(a, durable)
                # Prepared survives EOF/deadline: restart consults status before
                # deciding. Never undo a possibly durable applied replacement.


def main():
    try:
        if len(sys.argv) == 4 and sys.argv[2] == 'apply':
            supervise_apply(sys.argv[1], sys.argv[3])
            return 0
        if len(sys.argv) != 6:
            return 2
        supervise(sys.argv[1], float(sys.argv[2]), sys.argv[3], sys.argv[4], float(sys.argv[5]))
        return 0
    except Exception:
        # No secret-bearing exception, path, argv or service log output.
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
