// SPDX-License-Identifier: AGPL-3.0-or-later

package benchmark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStrictBoundedLoad(t *testing.T) {
	s, _ := openTest(t)
	j := startTest(t, s, "worker")
	r := s.jobs[j.ID]
	base, err := encode(s.jobs)
	if err != nil {
		t.Fatal(err)
	}
	mutated := func(change func(*record)) []byte {
		copy := r
		change(&copy)
		b, err := json.Marshal(diskState{Version: SchemaVersion, Jobs: []record{copy}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := map[string][]byte{
		"empty":                 nil,
		"truncated":             base[:len(base)-1],
		"trailing":              append(bytes.Clone(base), []byte(` {}`)...),
		"oversize":              bytes.Repeat([]byte(" "), maxFileBytes+1),
		"missing-jobs":          []byte(`{"version":1}`),
		"missing-version":       []byte(`{"jobs":[]}`),
		"future-version":        []byte(`{"version":2,"jobs":[]}`),
		"null-jobs":             []byte(`{"version":1,"jobs":null}`),
		"unknown-top":           []byte(`{"version":1,"jobs":[],"command":"no"}`),
		"duplicate-top":         []byte(`{"version":1,"version":1,"jobs":[]}`),
		"duplicate-nested":      bytes.Replace(base, []byte(`"parallel":2`), []byte(`"parallel":2,"parallel":2`), 1),
		"case-alias":            bytes.Replace(base, []byte(`"parallel":2`), []byte(`"Parallel":2`), 1),
		"unknown-option":        bytes.Replace(base, []byte(`"parallel":2`), []byte(`"parallel":2,"command":"no"`), 1),
		"null-option":           bytes.Replace(base, []byte(`"parallel":2`), []byte(`"parallel":null`), 1),
		"null-record":           []byte(`{"version":1,"jobs":[null]}`),
		"wrong-machine":         mutated(func(r *record) { r.Job.Provenance.WorkerID = "other" }),
		"wrong-hash":            mutated(func(r *record) { r.Job.Provenance.ModelSHA256 = strings.Repeat("c", 64) }),
		"wrong-policy":          mutated(func(r *record) { r.Job.Provenance.PolicyVersion++ }),
		"wrong-fixture":         mutated(func(r *record) { r.Job.Provenance.FixtureVersion++ }),
		"bad-deadline":          mutated(func(r *record) { r.Job.Provenance.DeadlineAt = r.Job.Provenance.CreatedAt }),
		"zero-time":             mutated(func(r *record) { r.Job.UpdatedAt = time.Time{} }),
		"changed-queued-time":   mutated(func(r *record) { r.Job.UpdatedAt = r.Job.UpdatedAt.Add(time.Second) }),
		"zero-numeric-option":   mutated(func(r *record) { r.Job.Options.Context = 0 }),
		"bad-enum":              mutated(func(r *record) { r.Job.State = "secret worker output" }),
		"bad-id":                mutated(func(r *record) { r.Job.ID = "../id" }),
		"running-without-lease": mutated(func(r *record) { r.Job.State, r.Job.Progress = Running, Preparing }),
		"queued-with-lease":     mutated(func(r *record) { r.Lease = strings.Repeat("a", 64) }),
		"fake-terminal":         mutated(func(r *record) { r.Job.State, r.Job.Progress, r.Job.FinishedAt = Completed, Done, r.Job.UpdatedAt }),
		"summary-on-queued":     mutated(func(r *record) { r.Job.Summary = summaryFor(r.Job) }),
		"oversized-record":      mutated(func(r *record) { r.Lease = strings.Repeat("a", maxRecordBytes) }),
	}
	duplicate, _ := json.Marshal(diskState{Version: SchemaVersion, Jobs: []record{r, r}})
	tests["duplicate-id"] = duplicate
	r2 := r
	r2.Job.ID = strings.Repeat("f", 64)
	sameMachine, _ := json.Marshal(diskState{Version: SchemaVersion, Jobs: []record{r, r2}})
	tests["duplicate-active-machine"] = sameMachine
	var records []record
	for i := range MaxJobs + 1 {
		copy := r
		copy.Job.ID = fmt.Sprintf("%064x", i)
		copy.Job.Machine = fmt.Sprintf("worker-%d", i)
		copy.Job.Provenance.WorkerID = copy.Job.Machine
		records = append(records, copy)
	}
	tests["too-many-jobs"], _ = json.Marshal(diskState{Version: SchemaVersion, Jobs: records})
	for i := range MaxHistory + 1 {
		records[i].Job.State, records[i].Job.Progress = Cancelled, Done
		records[i].Job.FinishedAt = records[i].Job.UpdatedAt
	}
	tests["too-much-history"], _ = json.Marshal(diskState{Version: SchemaVersion, Jobs: records[:MaxHistory+1]})
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(data); err != ErrCorrupt {
				t.Fatalf("corruption accepted: %v", err)
			}
		})
	}
	if jobs, err := decode(base); err != nil || len(jobs) != 1 {
		t.Fatal("valid state rejected", err)
	}
}

func TestOpenRejectsMissingCorruptPermissiveAndSymlinkState(t *testing.T) {
	for _, name := range []string{"missing", "corrupt", "oversize", "permissive-file", "permissive-dir", "symlink-file", "symlink-dir", "symlink-lock"} {
		t.Run(name, func(t *testing.T) {
			s, dir := openTest(t)
			startTest(t, s, "worker")
			_ = s.Close()
			benchDir := filepath.Join(dir, "benchmark")
			path := filepath.Join(benchDir, stateName)
			var err error
			want := ErrStorage
			switch name {
			case "missing":
				err, want = os.Remove(path), ErrCorrupt
			case "corrupt":
				err, want = os.WriteFile(path, []byte(`{"version":1}`), 0600), ErrCorrupt
			case "oversize":
				err, want = os.WriteFile(path, bytes.Repeat([]byte(" "), maxFileBytes+1), 0600), ErrCorrupt
			case "permissive-file":
				err = os.Chmod(path, 0644)
			case "permissive-dir":
				err = os.Chmod(benchDir, 0755)
			case "symlink-file":
				if err = os.Rename(path, path+"-original"); err == nil {
					err = os.Symlink(path+"-original", path)
				}
			case "symlink-dir":
				if err = os.Rename(benchDir, benchDir+"-original"); err == nil {
					err = os.Symlink(benchDir+"-original", benchDir)
				}
			case "symlink-lock":
				path = filepath.Join(benchDir, "writer.lock")
				if err = os.Rename(path, path+"-original"); err == nil {
					err = os.Symlink(path+"-original", path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			opened, err := Open(dir)
			if err != want || opened != nil {
				t.Fatalf("Open = %v, want %v", err, want)
			}
		})
	}
}

func TestRepeatedRecoveryRestartRetainsLeaseAndSequence(t *testing.T) {
	s, dir := openTest(t)
	j := startTest(t, s, "worker")
	a := leaseTest(t, s, "worker")
	if _, err := s.Update("worker", j.ID, a.Token, Update{Sequence: 1, State: Running, Progress: Measuring}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = s.Close()
		var err error
		s, err = Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		recovery := leaseTest(t, s, "worker")
		if recovery.Token != a.Token || !recovery.RecoveryOnly || recovery.Job.Sequence != 1 || recovery.Job.Provenance != j.Provenance {
			t.Fatal("restart lost recovery provenance/sequence/token")
		}
		fence(t, s, "worker", true)
	}
	defer s.Close()
	if _, err := s.Update("worker", j.ID, a.Token, Update{Sequence: 1, State: Cancelled, Progress: Done, Restored: true}); err != ErrReplay {
		t.Fatal("replay after restart accepted", err)
	}
	if _, err := s.Update("worker", j.ID, a.Token, Update{Sequence: 2, State: Cancelled, Progress: Done, Restored: true}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fence(t, s, "worker", false)
}

func TestRestartExpiresQueuedAndLeasedJobs(t *testing.T) {
	s, dir := openTest(t)
	old := time.Now().UTC().Add(-time.Hour)
	s.now = func() time.Time { return old }
	queued := startTest(t, s, "queued")
	leased := startTest(t, s, "leased")
	leaseTest(t, s, "leased")
	_ = s.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	q, err := s.Get("queued", queued.ID)
	if err != nil || q.State != Failed || q.Failure != FailureDeadline {
		t.Fatal("queued deadline not persisted on open", err)
	}
	l, err := s.Get("leased", leased.ID)
	if err != nil || l.State != RecoveryRequired || l.Failure != FailureDeadline {
		t.Fatal("leased deadline not recovered on open", err)
	}
	fence(t, s, "queued", false)
	fence(t, s, "leased", true)
}

func TestUncertainTerminalWriteCannotClearInMemoryFence(t *testing.T) {
	for _, written := range []bool{false, true} {
		t.Run(fmt.Sprint(written), func(t *testing.T) {
			s, dir := openTest(t)
			j := startTest(t, s, "worker")
			a := leaseTest(t, s, "worker")
			persist := s.persist
			s.persist = func(data []byte) error {
				if written {
					if err := persist(data); err != nil {
						t.Fatal(err)
					}
				}
				return ErrStorage
			}
			if _, err := s.Update("worker", j.ID, a.Token, completeFor(a.Job)); err != ErrPoisoned {
				t.Fatal(err)
			}
			if fenced, err := s.Fenced("worker"); !fenced || err != ErrPoisoned {
				t.Fatal("uncertain completion released admission", err)
			}
			_ = s.Close()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// A durable restored completion can release after reopen; an old
			// running record MUST recover and remain fenced instead.
			fence(t, s, "worker", !written)
		})
	}
}

func TestLazyExpiryFailureIsPoisoned(t *testing.T) {
	s, _ := openTest(t)
	j := startTest(t, s, "worker")
	leaseTest(t, s, "worker")
	s.now = func() time.Time { return j.Provenance.DeadlineAt }
	s.persist = func([]byte) error { return ErrStorage }
	if fenced, err := s.Fenced("worker"); !fenced || err != ErrPoisoned {
		t.Fatal("expiry persistence failed open", err)
	}
}

func TestOpenRejectsUnwritableRecoveryDirectory(t *testing.T) {
	s, dir := openTest(t)
	startTest(t, s, "worker")
	leaseTest(t, s, "worker")
	_ = s.Close()
	// The ledger is valid, but the recovery directory is not writable.
	// Open must reject it before returning any usable store.
	if err := os.Chmod(filepath.Join(dir, "benchmark"), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "benchmark"), 0700)
	if got, err := Open(dir); got != nil || err != ErrStorage {
		t.Fatal("Open returned unreconciled store", err)
	}
}

// A bounded decoder fuzz target: no storage, network, processes or secrets.
func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"version":1,"jobs":[]}`))
	f.Add([]byte(`{"version":1,"jobs":[null]}`))
	f.Add([]byte(`{"version":1,"version":1,"jobs":[]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		jobs, err := decode(b)
		if err != nil {
			if err != ErrCorrupt {
				t.Fatal("unsanitized decoder error")
			}
			return
		}
		encoded, err := encode(jobs)
		if err != nil {
			t.Fatal("decoded state cannot be encoded", err)
		}
		if _, err := decode(encoded); err != nil {
			t.Fatal("round trip failed", err)
		}
	})
}
