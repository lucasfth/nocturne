// SPDX-License-Identifier: AGPL-3.0-or-later
package benchmark

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func completedApplyFixture(t *testing.T, s *Store) (Job, ApplyTarget, string) {
	t.Helper()
	o := testOptions()
	target := ApplyTarget{ApplyEnabled: true, ModelSHA256: o.ModelSHA256, BuildDigest: o.BuildDigest, Audit: Audit{Actor: "admin-bearer", CodeRevision: strings.Repeat("c", 40), ConfigGeneration: strings.Repeat("d", 64)}}
	j, err := s.StartWithProvenance("worker-1", o, target.Audit)
	if err != nil {
		t.Fatal(err)
	}
	a := leaseTest(t, s, j.Machine)
	j, err = s.Update(j.Machine, j.ID, a.Token, completeFor(j))
	if err != nil {
		t.Fatal(err)
	}
	return j, target, a.Token
}

func TestApplyExplicitBindingAndReplay(t *testing.T) {
	s, dir := openTest(t)
	prior, target, old := completedApplyFixture(t, s)
	fence(t, s, prior.Machine, false)
	if _, err := s.Lease(prior.Machine); err != ErrNoWork {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ApplyTarget){
		func(a *ApplyTarget) { a.ApplyEnabled = false },
		func(a *ApplyTarget) { a.Audit.ConfigGeneration = strings.Repeat("e", 64) },
		func(a *ApplyTarget) { a.Audit.CodeRevision = strings.Repeat("e", 40) },
		func(a *ApplyTarget) { a.ModelSHA256 = strings.Repeat("e", 64) },
		func(a *ApplyTarget) { a.BuildDigest = strings.Repeat("e", 64) },
		func(a *ApplyTarget) { a.Audit.ConfigGeneration = "" },
	} {
		bad := target
		mutate(&bad)
		if _, err := s.RequestApply(prior.Machine, prior.ID, bad); err == nil {
			t.Fatal("accepted unsupported/stale")
		}
		fence(t, s, prior.Machine, false)
	}
	j, err := s.RequestApply(prior.Machine, prior.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	fence(t, s, j.Machine, true)
	if j.ApplyRequest.CompletedSequence != prior.Sequence || !j.ApplyRequest.CompletedAt.Equal(prior.FinishedAt) || !reflect.DeepEqual(j.Summary, prior.Summary) || j.Provenance != prior.Provenance {
		t.Fatal("lost evidence")
	}
	j.ApplyRequest.Audit.Actor = "mutated"
	a := leaseTest(t, s, j.Machine)
	if a.Action != ActionApply || a.RecoveryOnly || a.Job.ApplyRequest.Audit.Actor != "admin-bearer" {
		t.Fatal(a)
	}
	u := Update{Action: ActionApply, Sequence: 1, State: Completed, Progress: Done, Result: Applied}
	if _, err = s.Update(j.Machine, j.ID, old, u); err != ErrLease {
		t.Fatal(err)
	}
	bad := u
	bad.Action = ""
	if _, err = s.Update(j.Machine, j.ID, a.Token, bad); err == nil {
		t.Fatal("legacy completion accepted")
	}
	if _, err = s.Update(j.Machine, j.ID, a.Token, u); err == nil {
		t.Fatal("legacy applied accepted")
	}
	reconcileTest(t, s, a, ApplyPrepared)
	fence(t, s, j.Machine, true)
	reconcileTest(t, s, a, ApplyApplied)
	fence(t, s, j.Machine, false)
	if _, err = s.Update(j.Machine, j.ID, a.Token, u); err == nil {
		t.Fatal("replayed result")
	}
	if _, err = s.RequestApply(j.Machine, j.ID, target); err == nil {
		t.Fatal("replayed apply")
	}
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Get(j.Machine, j.ID)
	if err != nil || got.Result != Applied || !reflect.DeepEqual(got.Summary, prior.Summary) {
		t.Fatal(got, err)
	}
}

func TestApplyRollbackRestartAndDeadline(t *testing.T) {
	for _, mode := range []string{"rollback", "failed-rollback", "queued-restart", "running-restart", "deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, dir := openTest(t)
			prior, target, _ := completedApplyFixture(t, s)
			j, err := s.RequestApply(prior.Machine, prior.ID, target)
			if err != nil {
				t.Fatal(err)
			}
			var a Assignment
			if mode != "queued-restart" {
				a = leaseTest(t, s, j.Machine)
			}
			switch mode {
			case "queued-restart", "running-restart":
				s.Close()
				s, err = Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				a = leaseTest(t, s, j.Machine)
				if mode == "queued-restart" {
					if a.RecoveryOnly || a.Job.State != Running {
						t.Fatal(a)
					}
				} else if !a.RecoveryOnly || a.Job.State != RecoveryRequired {
					t.Fatal(a)
				}
			case "deadline":
				s.now = func() time.Time { return j.ApplyRequest.DeadlineAt }
				a = leaseTest(t, s, j.Machine)
				if !a.RecoveryOnly {
					t.Fatal("deadline not recovering")
				}
			case "cancel":
				if _, err = s.Cancel(j.Machine, j.ID); err != nil {
					t.Fatal(err)
				}
				a = leaseTest(t, s, j.Machine)
			case "failed-rollback":
				u := Update{Action: ActionApply, Sequence: 1, State: RecoveryRequired, Progress: Restoring, Failure: FailureRestoration}
				if _, err = s.Update(j.Machine, j.ID, a.Token, u); err != nil {
					t.Fatal(err)
				}
				a = leaseTest(t, s, j.Machine)
			}
			fence(t, s, j.Machine, true)
			seq := a.Job.Sequence + 1
			if a.RecoveryOnly {
				if _, err = s.Update(j.Machine, j.ID, a.Token, Update{Action: ActionApply, Sequence: seq, State: Completed, Progress: Done, Result: Applied}); err == nil {
					t.Fatal("recovery applied")
				}
			}
			u := Update{Action: ActionApply, Sequence: seq, State: Failed, Progress: Done, Failure: FailureExecution, Result: RolledBack, Restored: true}
			bad := u
			bad.Restored = false
			if _, err = s.Update(j.Machine, j.ID, a.Token, bad); err == nil {
				t.Fatal("unverified rollback")
			}
			if _, err = s.Update(j.Machine, j.ID, a.Token, u); err == nil {
				t.Fatal("legacy rollback accepted")
			}
			reconcileTest(t, s, a, ApplyRolledBack)
			fence(t, s, j.Machine, false)
			s.Close()
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err := reopened.Get(j.Machine, j.ID)
			if err != nil || got.Result != RolledBack || !reflect.DeepEqual(got.Summary, prior.Summary) {
				t.Fatal(got, err)
			}
		})
	}
}

func TestApplyPersistenceFailureFences(t *testing.T) {
	s, _ := openTest(t)
	j, target, _ := completedApplyFixture(t, s)
	s.persist = func([]byte) error { return errors.New("injected") }
	if _, err := s.RequestApply(j.Machine, j.ID, target); err != ErrPoisoned {
		t.Fatal(err)
	}
	if fenced, err := s.Fenced(j.Machine); !fenced || err != ErrPoisoned {
		t.Fatal(fenced, err)
	}
}
