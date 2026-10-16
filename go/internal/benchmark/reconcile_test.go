// SPDX-License-Identifier: AGPL-3.0-or-later
package benchmark

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func reconciliation(a Assignment, phase ApplyPhase) Reconciliation {
	return Reconciliation{TransactionID: a.Job.ApplyRequest.TransactionID, Generation: a.Job.ApplyRequest.Generation, Phase: phase, Durable: true}
}
func reconcileTest(t *testing.T, s *Store, a Assignment, phase ApplyPhase) Job {
	t.Helper()
	j, err := s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Token, reconciliation(a, phase))
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func applyFixture(t *testing.T, s *Store) Assignment {
	t.Helper()
	j, target, _ := completedApplyFixture(t, s)
	if _, err := s.RequestApply(j.Machine, j.ID, target); err != nil {
		t.Fatal(err)
	}
	return leaseTest(t, s, j.Machine)
}

func TestReconcileRestartReplayAndCancellation(t *testing.T) {
	for _, phase := range []ApplyPhase{ApplyApplied, ApplyRolledBack} {
		t.Run(string(phase), func(t *testing.T) {
			s, dir := openTest(t)
			a := applyFixture(t, s)
			u := reconciliation(a, ApplyApplied)
			if _, err := s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Token, u); err != ErrTransition {
				t.Fatal("unprepared applied", err)
			}
			prepared := reconcileTest(t, s, a, ApplyPrepared)
			if _, err := s.Update(a.Job.Machine, a.Job.ID, a.Token, Update{Action: ActionApply, Sequence: prepared.Sequence + 1, State: RecoveryRequired, Progress: Restoring, Failure: FailureRestoration}); err != ErrInvalidUpdate {
				t.Fatal("prepared decision overwritten", err)
			}
			if prepared.Result != "" || prepared.State.Terminal() {
				t.Fatal(prepared)
			}
			s.now = func() time.Time { return a.Job.ApplyRequest.DeadlineAt.Add(time.Hour) }
			if got, err := s.Cancel(a.Job.Machine, a.Job.ID); err != nil || !reflect.DeepEqual(got, prepared) {
				t.Fatal(got, err)
			}
			fence(t, s, a.Job.Machine, true)
			s.Close()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			recovered := leaseTest(t, s, a.Job.Machine)
			if !recovered.RecoveryOnly || recovered.Token != a.Token || recovered.Job.ApplyPhase != ApplyPrepared {
				t.Fatal("lost prepared reconciliation")
			}
			got := reconcileTest(t, s, a, ApplyPrepared)
			if !reflect.DeepEqual(got, prepared) {
				t.Fatal("replay changed prepared")
			}
			terminal := reconcileTest(t, s, a, phase)
			fence(t, s, a.Job.Machine, false)
			s.Close()
			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got = reconcileTest(t, s, a, phase); !reflect.DeepEqual(got, terminal) {
				t.Fatal("terminal replay changed decision")
			}
			other := ApplyApplied
			if phase == other {
				other = ApplyRolledBack
			}
			if _, err = s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Token, reconciliation(a, other)); err != ErrTransition {
				t.Fatal(err)
			}
		})
	}
}

func TestReconcileValidationAndRaces(t *testing.T) {
	s, _ := openTest(t)
	a := applyFixture(t, s)
	for _, change := range []func(*Reconciliation){
		func(u *Reconciliation) { u.Generation++ }, func(u *Reconciliation) { u.TransactionID = a.Job.ID }, func(u *Reconciliation) { u.Durable = false }, func(u *Reconciliation) { u.Phase = "unknown" },
	} {
		u := reconciliation(a, ApplyPrepared)
		change(&u)
		if _, err := s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Token, u); err == nil {
			t.Fatal("invalid accepted")
		}
	}
	if _, err := s.ReconcileApply("other", a.Job.ID, a.Token, reconciliation(a, ApplyPrepared)); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, err := s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Job.ID, reconciliation(a, ApplyPrepared)); err != ErrLease {
		t.Fatal(err)
	}
	reconcileTest(t, s, a, ApplyPrepared)
	// Concurrent duplicate and conflicting decisions serialize; exactly one wins.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			phase := ApplyApplied
			if i%2 == 0 {
				phase = ApplyRolledBack
			}
			_, err := s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Token, reconciliation(a, phase))
			if err != nil && err != ErrTransition {
				t.Error(err)
			}
			_, _ = s.Get(a.Job.Machine, a.Job.ID)
			_, _ = s.Fenced(a.Job.Machine)
		})
	}
	wg.Wait()
	j, err := s.Get(a.Job.Machine, a.Job.ID)
	if err != nil || !j.State.Terminal() {
		t.Fatal(j, err)
	}
	fence(t, s, a.Job.Machine, false)
}

func TestReconcileUncertainWrites(t *testing.T) {
	for _, phase := range []ApplyPhase{ApplyPrepared, ApplyApplied, ApplyRolledBack} {
		for _, after := range []bool{false, true} {
			t.Run(string(phase)+map[bool]string{false: "-before", true: "-after"}[after], func(t *testing.T) {
				s, dir := openTest(t)
				a := applyFixture(t, s)
				if phase != ApplyPrepared {
					reconcileTest(t, s, a, ApplyPrepared)
				}
				persist := s.persist
				s.persist = func(b []byte) error {
					if after {
						if err := persist(b); err != nil {
							return err
						}
					}
					return errors.New("injected")
				}
				if _, err := s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Token, reconciliation(a, phase)); err != ErrPoisoned {
					t.Fatal(err)
				}
				if f, err := s.Fenced(a.Job.Machine); !f || err != ErrPoisoned {
					t.Fatal(f, err)
				}
				s.Close()
				s, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if phase == ApplyPrepared && !after {
					// No prepare decision survived: restart requires rollback, never apply.
					if _, err = s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Token, reconciliation(a, ApplyApplied)); err != ErrTransition {
						t.Fatal(err)
					}
					reconcileTest(t, s, a, ApplyRolledBack)
				} else {
					reconcileTest(t, s, a, phase)
				}
			})
		}
	}
}
