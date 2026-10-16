// SPDX-License-Identifier: AGPL-3.0-or-later
package benchmark

import (
	"crypto/subtle"
	"maps"
)

func validApplyPhase(j Job, lease string) bool {
	switch j.ApplyPhase {
	case "":
		return j.Result == ""
	case ApplyPrepared:
		return lease != "" && j.State == Running && j.Progress == Validating && j.Sequence > 0 && j.Sequence < MaxSequence && j.Failure == NoFailure
	case ApplyApplied:
		return lease != "" && j.State == Completed && j.Result == Applied && j.Sequence >= 2
	case ApplyRolledBack:
		return lease != "" && j.State == Failed && j.Result == RolledBack
	}
	return false
}

// ReconcileApply requires independently authenticated machine credentials on EVERY
// invocation, even idempotent replay. A lease alone never authenticates a worker.
// The worker's durable final decision is irreversible before this call; ambiguous
// responses require status/replay, NOT rollback. See README.md.
func (s *Store) ReconcileApply(machine, id, lease string, u Reconciliation) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepare(machine); err != nil {
		return Job{}, err
	}
	r, err := s.lookup(machine, id)
	if err != nil {
		return Job{}, err
	}
	if !validHex(lease, 64) || r.Lease == "" || subtle.ConstantTimeCompare([]byte(lease), []byte(r.Lease)) != 1 {
		return Job{}, ErrLease
	}
	a := r.Job.ApplyRequest
	if r.Job.Action != ActionApply || a == nil || u.TransactionID != a.TransactionID || u.Generation != a.Generation {
		return Job{}, ErrTransition
	}
	if !u.Durable || (u.Phase != ApplyPrepared && u.Phase != ApplyApplied && u.Phase != ApplyRolledBack) {
		return Job{}, ErrInvalidUpdate
	}
	j := &r.Job
	// Matching replay performs no write and cannot resurrect an old decision.
	if j.ApplyPhase == u.Phase {
		return snapshot(*j), nil
	}
	if j.State.Terminal() || j.State == Queued {
		return Job{}, ErrTransition
	}
	switch u.Phase {
	case ApplyPrepared:
		if j.State != Running || j.ApplyPhase != "" || j.Progress == Restoring || j.Sequence >= MaxSequence-1 {
			return Job{}, ErrTransition
		}
		j.Progress = Validating
	case ApplyApplied:
		if j.ApplyPhase != ApplyPrepared {
			return Job{}, ErrTransition
		}
		j.State, j.Progress, j.Result, j.Failure = Completed, Done, Applied, NoFailure
	case ApplyRolledBack:
		j.State, j.Progress, j.Result, j.Failure, j.Restored = Failed, Done, RolledBack, FailureExecution, true
	}
	j.ApplyPhase = u.Phase
	j.Sequence++
	j.UpdatedAt = timestamp(s.now(), *j)
	if j.State.Terminal() {
		j.FinishedAt = j.UpdatedAt
	}
	next := maps.Clone(s.jobs)
	next[id] = r
	if err := s.commit(next); err != nil {
		return Job{}, err
	}
	return snapshot(*j), nil
}
