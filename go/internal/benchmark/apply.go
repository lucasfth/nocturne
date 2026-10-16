// SPDX-License-Identifier: AGPL-3.0-or-later
package benchmark

import (
	"maps"
	"time"
)

// ApplyTarget must come from trusted startup discovery, never an HTTP body.
type ApplyTarget struct {
	ApplyEnabled             bool
	ModelSHA256, BuildDigest string
	Audit                    Audit
}

func deadline(j Job) time.Time {
	if j.ApplyRequest != nil {
		return j.ApplyRequest.DeadlineAt
	}
	return j.Provenance.DeadlineAt
}

// RequestApply atomically reserves the fence and consumes this completed job's
// single apply opportunity. Legacy Apply remains unsupported without a binding.
func (s *Store) RequestApply(machine, id string, target ApplyTarget) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepare(machine); err != nil {
		return Job{}, err
	}
	r, err := s.lookup(machine, id)
	if err != nil {
		return Job{}, err
	}
	if !target.ApplyEnabled {
		return Job{}, ErrUnsupported
	}
	j := &r.Job
	a := target.Audit
	if !a.valid() || a.Actor == "" || a.CodeRevision == "" || a.ConfigGeneration == "" ||
		a.CodeRevision != j.Provenance.CodeRevision || a.ConfigGeneration != j.Provenance.ConfigGeneration ||
		target.ModelSHA256 != j.Options.ModelSHA256 || target.BuildDigest != j.Options.BuildDigest {
		return Job{}, ErrTransition
	}
	if j.Action != "" || j.State != Completed || !j.Restored || j.Summary == nil {
		return Job{}, ErrTransition
	}
	if _, ok := s.active[machine]; ok {
		return Job{}, ErrBusy
	}
	tx, err := s.token()
	if err != nil {
		return Job{}, err
	}
	now := timestamp(s.now(), *j)
	j.ApplyRequest = &ApplyRequest{TransactionID: tx, Generation: 1, Audit: a, RequestedAt: now, DeadlineAt: now.Add(time.Duration(j.Options.BudgetSeconds) * time.Second), CompletedAt: j.FinishedAt, CompletedSequence: j.Sequence}
	j.Action, j.Result = ActionApply, ""
	j.State, j.Progress, j.Sequence, j.Restored = Queued, Pending, 0, false
	j.FinishedAt, j.UpdatedAt = time.Time{}, now
	// No capability exists until Lease durably records dispatch.
	r.Lease = ""
	next := maps.Clone(s.jobs)
	next[id] = r
	if err := s.commit(next); err != nil {
		return Job{}, err
	}
	return snapshot(*j), nil
}

func validateApplyUpdate(j Job, u Update) error {
	// Terminal decisions must use ReconcileApply, never the legacy attestation.
	if u.State.Terminal() || j.ApplyPhase != "" || u.Action != ActionApply || u.Summary != nil {
		return ErrInvalidUpdate
	}
	if j.State.Terminal() || j.State == Queued {
		return ErrTransition
	}
	if u.Sequence == 0 || u.Sequence > MaxSequence || u.Sequence != j.Sequence+1 {
		return ErrReplay
	}
	if u.Result != "" || u.Restored {
		return ErrInvalidUpdate
	}
	// Reuse benchmark progress/sequence rules, but not its completion contract.
	j.Action, j.ApplyRequest, j.Summary = "", nil, nil
	u.Action = ""
	return validateUpdate(j, u)
}

func validApplyRecord(r record) bool {
	j := r.Job
	a := j.ApplyRequest
	if a == nil || !validHex(a.TransactionID, 64) || a.Generation != 1 || !validApplyPhase(j, r.Lease) || !a.Audit.valid() || a.Audit.Actor == "" || a.Audit.CodeRevision == "" || a.Audit.ConfigGeneration == "" ||
		a.Audit.CodeRevision != j.Provenance.CodeRevision || a.Audit.ConfigGeneration != j.Provenance.ConfigGeneration ||
		!validTime(a.RequestedAt) || !validTime(a.DeadlineAt) || a.RequestedAt.Before(a.CompletedAt) ||
		!a.DeadlineAt.Equal(a.RequestedAt.Add(time.Duration(j.Options.BudgetSeconds)*time.Second)) ||
		j.UpdatedAt.Before(a.RequestedAt) || (r.Lease != "" && !validHex(r.Lease, 64)) {
		return false
	}
	// Validate the immutable prior completion using the legacy strict validator.
	prior := r
	// The prior completion had a lease, but its secret is deliberately retired.
	// Supply a structurally valid placeholder only for evidence validation.
	prior.Lease = j.ID
	prior.Job.Action, prior.Job.Result, prior.Job.ApplyRequest = "", "", nil
	prior.Job.ApplyPhase = ""
	prior.Job.State, prior.Job.Progress, prior.Job.Failure, prior.Job.Restored = Completed, Done, NoFailure, true
	prior.Job.Sequence = a.CompletedSequence
	prior.Job.FinishedAt, prior.Job.UpdatedAt = a.CompletedAt, a.CompletedAt
	if !validRecord(prior) || j.Sequence > MaxSequence {
		return false
	}
	if r.Lease == "" {
		if j.Sequence != 0 || j.Restored || j.Result != "" {
			return false
		}
		switch j.State {
		case Queued:
			return j.Progress == Pending && j.Failure == NoFailure && j.FinishedAt.IsZero() && j.UpdatedAt.Equal(a.RequestedAt)
		case Cancelled, Failed:
			return j.Progress == Done && validTime(j.FinishedAt) && j.FinishedAt.Equal(j.UpdatedAt) &&
				((j.State == Cancelled && j.Failure == NoFailure) ||
					(j.State == Failed && j.Failure == FailureDeadline && !j.UpdatedAt.Before(a.DeadlineAt)))
		default:
			return false
		}
	}
	if j.State.Terminal() {
		if j.Sequence == 0 || j.Progress != Done || !validTime(j.FinishedAt) || !j.FinishedAt.Equal(j.UpdatedAt) {
			return false
		}
		if j.Result == Applied {
			return j.State == Completed && !j.Restored && j.Failure == NoFailure
		}
		return j.Result == RolledBack && j.Restored && ((j.State == Failed && validFailure(j.Failure)) || (j.State == Cancelled && j.Failure == NoFailure))
	}
	if j.Result != "" || j.Restored || !j.FinishedAt.IsZero() || j.Sequence >= MaxSequence {
		return false
	}
	switch j.State {
	case Queued:
		return j.Sequence == 0 && j.Progress == Pending && j.Failure == NoFailure && j.UpdatedAt.Equal(a.RequestedAt)
	case Running:
		return stage(j.Progress) >= 1 && stage(j.Progress) <= 6 && j.Failure == NoFailure
	case Cancelling:
		return j.Progress == Restoring && j.Failure == NoFailure
	case RecoveryRequired:
		return j.Progress == Restoring && validFailure(j.Failure)
	}
	return false
}
