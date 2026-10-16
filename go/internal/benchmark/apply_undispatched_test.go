// SPDX-License-Identifier: AGPL-3.0-or-later
package benchmark

import (
	"reflect"
	"testing"
	"time"
)

func TestApplyUndispatchedTerminal(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "restart-expired"} {
		t.Run(mode, func(t *testing.T) {
			s, dir := openTest(t)
			if mode == "restart-expired" {
				s.now = func() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }
			}
			prior, target, old := completedApplyFixture(t, s)
			j, err := s.RequestApply(prior.Machine, prior.ID, target)
			if err != nil {
				t.Fatal(err)
			}
			if s.jobs[j.ID].Lease != "" {
				t.Fatal("queued capability")
			}
			if _, err := s.Update(j.Machine, j.ID, old, Update{Action: ActionApply, Sequence: 1, State: Completed, Progress: Done, Result: Applied}); err != ErrLease {
				t.Fatal(err)
			}
			switch mode {
			case "cancel":
				j, err = s.Cancel(j.Machine, j.ID)
			case "deadline":
				s.now = func() time.Time { return j.ApplyRequest.DeadlineAt }
				j, err = s.Get(j.Machine, j.ID)
			case "restart-expired":
				s.Close()
				s, err = Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				j, err = s.Get(j.Machine, j.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			want, failure := Failed, FailureDeadline
			if mode == "cancel" {
				want, failure = Cancelled, NoFailure
			}
			if j.State != want || j.Failure != failure || j.Progress != Done || j.Sequence != 0 || j.Restored || j.Result != "" || !reflect.DeepEqual(j.Summary, prior.Summary) {
				t.Fatal(j)
			}
			fence(t, s, j.Machine, false)
			if _, err := s.Lease(j.Machine); err != ErrNoWork {
				t.Fatal(err)
			}
			if _, err := s.RequestApply(j.Machine, j.ID, target); err != ErrTransition {
				t.Fatal(err)
			}
			again, err := s.Cancel(j.Machine, j.ID)
			if err != nil || !reflect.DeepEqual(again, j) {
				t.Fatal(again, err)
			}
			s.Close()
			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.Get(j.Machine, j.ID)
			if err != nil || !reflect.DeepEqual(got, j) {
				t.Fatal(got, err)
			}
			fence(t, s, j.Machine, false)
		})
	}
}

func TestApplyUnleasedRecordInvariants(t *testing.T) {
	s, _ := openTest(t)
	prior, target, _ := completedApplyFixture(t, s)
	j, err := s.RequestApply(prior.Machine, prior.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	queued := s.jobs[j.ID]
	if _, err = s.Cancel(j.Machine, j.ID); err != nil {
		t.Fatal(err)
	}
	terminal := s.jobs[j.ID]
	for _, base := range []record{queued, terminal} {
		data, err := encode(map[string]record{j.ID: base})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decode(data); err != nil {
			t.Fatal(err)
		}
		for _, mutate := range []func(*record){
			func(r *record) { r.Job.Restored = true },
			func(r *record) { r.Job.Result = RolledBack },
			func(r *record) { r.Job.Sequence = 1 },
			func(r *record) { r.Job.State = Running },
			func(r *record) { r.Job.State = RecoveryRequired },
			func(r *record) { r.Job.State = Failed; r.Job.Failure = FailureExecution },
			func(r *record) { r.Job.State = Failed; r.Job.Failure = FailureDeadline },
		} {
			bad := base
			mutate(&bad)
			if validRecord(bad) {
				t.Fatal("accepted invalid undispatched record")
			}
		}
	}
}
