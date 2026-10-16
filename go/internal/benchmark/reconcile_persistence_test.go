// SPDX-License-Identifier: AGPL-3.0-or-later
package benchmark

import (
	"strings"
	"testing"
)

func TestLegacyApplyLedgerFailsClosed(t *testing.T) {
	s, dir := openTest(t)
	a := applyFixture(t, s)
	raw, err := encode(s.jobs)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(raw), `"transaction_id":"`+a.Job.ApplyRequest.TransactionID+`","generation":1,`, "", 1)
	if legacy == string(raw) {
		t.Fatal("fixture not changed")
	}
	if err = s.writeAtomic([]byte(legacy)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if reopened, err := Open(dir); err != ErrCorrupt {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("legacy Apply admitted", err)
	}
}

func TestReconcileSequenceReservationAndCorruptPhase(t *testing.T) {
	s, _ := openTest(t)
	a := applyFixture(t, s)
	// Boundary fixture avoids thousands of redundant fsyncs.
	r := s.jobs[a.Job.ID]
	r.Job.Sequence = MaxSequence - 1
	s.jobs[a.Job.ID] = r
	if _, err := s.ReconcileApply(a.Job.Machine, a.Job.ID, a.Token, reconciliation(a, ApplyPrepared)); err != ErrTransition {
		t.Fatal("no final sequence reserved", err)
	}
	r.Job.Sequence = MaxSequence - 2
	s.jobs[a.Job.ID] = r
	prepared := reconcileTest(t, s, a, ApplyPrepared)
	if prepared.Sequence != MaxSequence-1 {
		t.Fatal(prepared.Sequence)
	}
	r = s.jobs[a.Job.ID]
	for _, change := range []func(*record){
		func(r *record) { r.Job.State = Completed },
		func(r *record) { r.Job.Result = Applied },
		func(r *record) { r.Job.ApplyPhase = "unknown" },
		func(r *record) { r.Job.Sequence = 0 },
		func(r *record) { r.Lease = "" },
	} {
		bad := r
		change(&bad)
		if validRecord(bad) {
			t.Fatal("corrupt phase accepted")
		}
	}
	terminal := reconcileTest(t, s, a, ApplyApplied)
	if terminal.Sequence != MaxSequence {
		t.Fatal(terminal.Sequence)
	}
	reconcileTest(t, s, a, ApplyApplied)
}
