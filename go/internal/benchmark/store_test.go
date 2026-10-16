// SPDX-License-Identifier: AGPL-3.0-or-later

package benchmark

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testOptions() Options {
	return Options{ModelSHA256: strings.Repeat("a", 64), BuildDigest: strings.Repeat("b", 64)}
}

func openTest(t testing.TB) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func startTest(t testing.TB, s *Store, machine string) Job {
	t.Helper()
	j, err := s.Start(machine, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func leaseTest(t testing.TB, s *Store, machine string) Assignment {
	t.Helper()
	a, err := s.Lease(machine)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func summaryFor(j Job) *Summary {
	return &Summary{
		Quality: QualityPass, Context: j.Options.Context, Parallel: j.Options.Parallel,
		PromptTokens: j.Options.Context - 100, OutputTokens: 100,
		DurationSeconds: 2, TokensPerSecond: 50, ModelVerified: true,
		ModelSHA256: j.Provenance.ModelSHA256, BuildDigest: j.Provenance.BuildDigest,
	}
}

func completeFor(j Job) Update {
	return Update{Sequence: j.Sequence + 1, State: Completed, Progress: Done, Restored: true, Summary: summaryFor(j)}
}

func fence(t testing.TB, s *Store, machine string, want bool) {
	t.Helper()
	got, err := s.Fenced(machine)
	if err != nil || got != want {
		t.Fatalf("fence = %v, %v; want %v", got, err, want)
	}
}

func TestOptions(t *testing.T) {
	o, err := testOptions().Normalize()
	if err != nil || o.Context != 32768 || o.Parallel != 2 || o.BudgetSeconds != 600 {
		t.Fatalf("defaults: %+v, %v", o, err)
	}
	for name, change := range map[string]func(*Options){
		"context-low":       func(o *Options) { o.Context = 8191 },
		"context-high":      func(o *Options) { o.Context = MaxContext + 1024 },
		"context-step":      func(o *Options) { o.Context = 8193 },
		"parallel-negative": func(o *Options) { o.Parallel = -1 },
		"parallel-high":     func(o *Options) { o.Parallel = MaxParallel + 1 },
		"budget-low":        func(o *Options) { o.BudgetSeconds = 29 },
		"budget-high":       func(o *Options) { o.BudgetSeconds = MaxBudgetSeconds + 1 },
		"hash-empty":        func(o *Options) { o.ModelSHA256 = "" },
		"hash-zero":         func(o *Options) { o.ModelSHA256 = strings.Repeat("0", 64) },
		"hash-case":         func(o *Options) { o.ModelSHA256 = strings.Repeat("A", 64) },
		"build-url":         func(o *Options) { o.BuildDigest = "https://invalid.example/model" },
		"build-command":     func(o *Options) { o.BuildDigest = "sh -c command" },
		"hash-path":         func(o *Options) { o.ModelSHA256 = "/tmp/model" },
	} {
		t.Run(name, func(t *testing.T) {
			o := testOptions()
			change(&o)
			if _, err := o.Normalize(); err != ErrInvalidOptions {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, context := range []int{MinContext, MaxContext} {
		o := testOptions()
		o.Context, o.Parallel, o.BudgetSeconds = context, MaxParallel, MaxBudgetSeconds
		if _, err := o.Normalize(); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := openTest(t)
	for _, machine := range []string{"", "../other", "https://host", "Upper", "worker_1", "-worker", strings.Repeat("a", 33), "worker\n"} {
		if _, err := s.Start(machine, testOptions()); err != ErrInvalidMachine {
			t.Fatalf("invalid machine accepted: %v", err)
		}
	}
}

func TestLifecycleAndSnapshots(t *testing.T) {
	s, dir := openTest(t)
	j := startTest(t, s, "worker-1")
	if j.State != Queued || j.Progress != Pending || !validHex(j.ID, 64) {
		t.Fatal("invalid initial job")
	}
	p := j.Provenance
	if p.WorkerID != "worker-1" || p.SchemaVersion != SchemaVersion || p.PolicyVersion != PolicyVersion || p.FixtureVersion != FixtureVersion || p.DeadlineAt.Sub(p.CreatedAt) != 600*time.Second {
		t.Fatal("invalid provenance")
	}
	fence(t, s, "worker-1", true)
	if _, err := s.Start("worker-1", testOptions()); err != ErrBusy {
		t.Fatal(err)
	}
	a := leaseTest(t, s, "worker-1")
	if a.Job.ID != j.ID || a.RecoveryOnly || a.Job.State != Running || !validHex(a.Token, 64) || a.Token == j.ID {
		t.Fatal("invalid lease")
	}
	if _, err := s.Lease("worker-1"); err != ErrNoWork {
		t.Fatal(err)
	}
	u := Update{Sequence: 1, State: Running, Progress: Measuring}
	j, err := s.Update("worker-1", j.ID, a.Token, u)
	if err != nil || j.Progress != Measuring {
		t.Fatal(err)
	}
	if _, err := s.Update("worker-1", j.ID, a.Token, u); err != ErrReplay {
		t.Fatal("replayed update accepted", err)
	}
	u.Sequence, u.Progress = 2, Loading
	if _, err := s.Update("worker-1", j.ID, a.Token, u); err != ErrInvalidUpdate {
		t.Fatal("backwards stage accepted", err)
	}
	completion := completeFor(j)
	j, err = s.Update("worker-1", j.ID, a.Token, completion)
	if err != nil || j.State != Completed || !j.Restored || j.Summary == nil || j.Provenance != p {
		t.Fatal("completion/provenance", err)
	}
	fence(t, s, "worker-1", false)
	if _, err := s.Update("worker-1", j.ID, a.Token, completion); err != ErrTransition {
		t.Fatal("terminal replay accepted", err)
	}
	completion.Summary.TokensPerSecond = 1234
	j.Summary.ModelVerified = false
	j.Options.Context = MinContext
	j.Provenance.ModelSHA256 = "changed"
	got, err := s.Get("worker-1", j.ID)
	if err != nil || !got.Summary.ModelVerified || got.Summary.TokensPerSecond != 50 || got.Provenance != p || got.Options.Context != DefaultContext {
		t.Fatal("snapshot mutation escaped", err)
	}
	list, err := s.List("worker-1")
	if err != nil || len(list) != 1 {
		t.Fatal("list", err)
	}
	list[0].Summary.ModelVerified = false
	if got, _ = s.Get("worker-1", j.ID); !got.Summary.ModelVerified {
		t.Fatal("list mutation escaped")
	}
	before := got
	if got, err = s.Cancel("worker-1", j.ID); err != nil || !reflect.DeepEqual(got, before) {
		t.Fatal("terminal cancellation rewrote history", err)
	}
	if err := s.Apply("worker-1", j.ID); err != ErrUnsupported {
		t.Fatal(err)
	}
	_ = s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err = reopened.Get("worker-1", j.ID)
	if err != nil || !reflect.DeepEqual(got, before) {
		t.Fatal("completed job changed on restart", err)
	}
	fence(t, reopened, "worker-1", false)
}

func TestCrossMachineAndLeaseBinding(t *testing.T) {
	s, _ := openTest(t)
	j := startTest(t, s, "worker-a")
	other := startTest(t, s, "worker-b")
	a, b := leaseTest(t, s, "worker-a"), leaseTest(t, s, "worker-b")
	if _, err := s.Get("worker-b", j.ID); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, err := s.Cancel("worker-b", j.ID); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, err := s.Update("worker-b", j.ID, a.Token, completeFor(a.Job)); err != ErrNotFound {
		t.Fatal(err)
	}
	for _, token := range []string{"", j.ID, b.Token, strings.Repeat("0", 64), strings.Repeat("a", 10000)} {
		if _, err := s.Update("worker-a", j.ID, token, completeFor(a.Job)); err != ErrLease {
			t.Fatal(err)
		}
	}
	list, err := s.List("worker-b")
	if err != nil || len(list) != 1 || list[0].ID != other.ID {
		t.Fatal("cross-machine list leak", err)
	}
	if _, err := s.Update("worker-a", j.ID, a.Token, completeFor(a.Job)); err != nil {
		t.Fatal(err)
	}
	next := startTest(t, s, "worker-a")
	newLease := leaseTest(t, s, "worker-a")
	if newLease.Token == a.Token {
		t.Fatal("lease reused")
	}
	if _, err := s.Update("worker-a", next.ID, a.Token, completeFor(newLease.Job)); err != ErrLease {
		t.Fatal("stale lease accepted", err)
	}
	fence(t, s, "worker-b", true)
}

func TestCancellationAndRestoration(t *testing.T) {
	s, _ := openTest(t)
	j := startTest(t, s, "worker")
	j, err := s.Cancel("worker", j.ID)
	if err != nil || j.State != Cancelled || j.Restored {
		t.Fatal(err)
	}
	fence(t, s, "worker", false)
	if _, err := s.Lease("worker"); err != ErrNoWork {
		t.Fatal(err)
	}
	j = startTest(t, s, "worker")
	a := leaseTest(t, s, "worker")
	j, err = s.Cancel("worker", j.ID)
	if err != nil || j.State != Cancelling {
		t.Fatal(err)
	}
	fence(t, s, "worker", true)
	again := leaseTest(t, s, "worker")
	if !again.RecoveryOnly || again.Token != a.Token {
		t.Fatal("cancellation must be restoration-only")
	}
	if _, err := s.Update("worker", j.ID, a.Token, completeFor(a.Job)); err != ErrTransition {
		t.Fatal("completion after cancellation accepted", err)
	}
	u := Update{Sequence: 1, State: Cancelled, Progress: Done}
	if _, err := s.Update("worker", j.ID, a.Token, u); err != ErrInvalidUpdate {
		t.Fatal("unrestored cancellation accepted", err)
	}
	fence(t, s, "worker", true)
	u.Restored = true
	j, err = s.Update("worker", j.ID, a.Token, u)
	if err != nil || j.State != Cancelled || !j.Restored {
		t.Fatal(err)
	}
	fence(t, s, "worker", false)
}

func TestRecoveryAndDeadline(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for _, cancel := range []bool{false, true} {
			t.Run(fmt.Sprintf("restart=%v/cancel=%v", restart, cancel), func(t *testing.T) {
				s, dir := openTest(t)
				j := startTest(t, s, "worker")
				a := leaseTest(t, s, "worker")
				if cancel {
					if _, err := s.Cancel("worker", j.ID); err != nil {
						t.Fatal(err)
					}
				}
				wantFailure := FailureDeadline
				if restart {
					_ = s.Close()
					var err error
					s, err = Open(dir)
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					wantFailure = FailureRestart
				} else {
					s.now = func() time.Time { return j.Provenance.DeadlineAt }
				}
				got, err := s.Get("worker", j.ID)
				if err != nil || got.State != RecoveryRequired || got.Failure != wantFailure || got.Provenance != j.Provenance {
					t.Fatalf("recovery = %+v, %v", got, err)
				}
				fence(t, s, "worker", true)
				if _, err := s.Start("worker", testOptions()); err != ErrBusy {
					t.Fatal(err)
				}
				r := leaseTest(t, s, "worker")
				if !r.RecoveryOnly || r.Token != a.Token {
					t.Fatal("recovery lease rotated or executable")
				}
				if got, err := s.Cancel("worker", j.ID); err != nil || got.State != RecoveryRequired {
					t.Fatal("cancel cleared recovery", err)
				}
				if _, err := s.Update("worker", j.ID, a.Token, completeFor(a.Job)); err != ErrTransition {
					t.Fatal("recovered work completed", err)
				}
				u := Update{Sequence: 1, State: Failed, Progress: Done, Failure: FailureExecution}
				if _, err := s.Update("worker", j.ID, a.Token, u); err != ErrInvalidUpdate {
					t.Fatal("failure cleared fence without restoration", err)
				}
				u.Restored = true
				if _, err := s.Update("worker", j.ID, a.Token, u); err != nil {
					t.Fatal(err)
				}
				fence(t, s, "worker", false)
			})
		}
	}
}

func TestQueuedRestartAndExpiry(t *testing.T) {
	s, dir := openTest(t)
	j := startTest(t, s, "worker")
	_ = s.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get("worker", j.ID)
	if err != nil || got.State != Queued {
		t.Fatal("queued job did not survive restart", err)
	}
	s.now = func() time.Time { return j.Provenance.DeadlineAt }
	got, err = s.Get("worker", j.ID)
	if err != nil || got.State != Failed || got.Failure != FailureDeadline || got.Restored {
		t.Fatal("queued expiry", err)
	}
	fence(t, s, "worker", false)
	if _, err := s.Lease("worker"); err != ErrNoWork {
		t.Fatal(err)
	}
}

func TestSummaryAndUpdateValidation(t *testing.T) {
	s, _ := openTest(t)
	j := startTest(t, s, "worker")
	a := leaseTest(t, s, "worker")
	for name, change := range map[string]func(*Update){
		"no-summary":              func(u *Update) { u.Summary = nil },
		"no-restoration":          func(u *Update) { u.Restored = false },
		"no-hash-verification":    func(u *Update) { u.Summary.ModelVerified = false },
		"wrong-hash":              func(u *Update) { u.Summary.ModelSHA256 = strings.Repeat("c", 64) },
		"wrong-build":             func(u *Update) { u.Summary.BuildDigest = strings.Repeat("c", 64) },
		"quality-fail":            func(u *Update) { u.Summary.Quality = QualityFail },
		"quality-unknown":         func(u *Update) { u.Summary.Quality = "arbitrary text" },
		"context":                 func(u *Update) { u.Summary.Context = MinContext },
		"parallel":                func(u *Update) { u.Summary.Parallel = 1 },
		"short-context-sample":    func(u *Update) { u.Summary.PromptTokens-- },
		"no-output":               func(u *Update) { u.Summary.OutputTokens = 0 },
		"integer-overflow":        func(u *Update) { u.Summary.PromptTokens = math.MaxInt },
		"no-duration":             func(u *Update) { u.Summary.DurationSeconds = 0 },
		"duration-over-budget":    func(u *Update) { u.Summary.DurationSeconds = 601 },
		"nan-duration":            func(u *Update) { u.Summary.DurationSeconds = math.NaN() },
		"inf-duration":            func(u *Update) { u.Summary.DurationSeconds = math.Inf(1) },
		"nan-throughput":          func(u *Update) { u.Summary.TokensPerSecond = math.NaN() },
		"inf-throughput":          func(u *Update) { u.Summary.TokensPerSecond = math.Inf(1) },
		"negative-throughput":     func(u *Update) { u.Summary.TokensPerSecond = -1 },
		"inconsistent-throughput": func(u *Update) { u.Summary.TokensPerSecond = 5000 },
		"state":                   func(u *Update) { u.State = "arbitrary text" },
		"progress":                func(u *Update) { u.Progress = "arbitrary text" },
		"failure":                 func(u *Update) { u.Failure = "secret error" },
		"summary-on-running":      func(u *Update) { u.State, u.Progress, u.Restored = Running, Measuring, false },
	} {
		t.Run(name, func(t *testing.T) {
			u := completeFor(a.Job)
			change(&u)
			if _, err := s.Update("worker", j.ID, a.Token, u); err != ErrInvalidUpdate {
				t.Fatalf("got %v", err)
			}
			fence(t, s, "worker", true)
			got, err := s.Get("worker", j.ID)
			if err != nil || got.Sequence != 0 || got.Summary != nil {
				t.Fatal("invalid update changed state", err)
			}
		})
	}
	for _, sequence := range []uint64{0, 2, MaxSequence + 1, math.MaxUint64} {
		u := completeFor(a.Job)
		u.Sequence = sequence
		if _, err := s.Update("worker", j.ID, a.Token, u); err != ErrReplay {
			t.Fatal(err)
		}
	}
	u := Update{Sequence: 1, State: RecoveryRequired, Progress: Restoring, Failure: FailureRestoration}
	if _, err := s.Update("worker", j.ID, a.Token, u); err != nil {
		t.Fatal(err)
	}
	fence(t, s, "worker", true)
	u = Update{Sequence: 2, State: Cancelled, Progress: Done, Restored: true}
	if _, err := s.Update("worker", j.ID, a.Token, u); err != nil {
		t.Fatal(err)
	}
	fence(t, s, "worker", false)
}

func TestWorkerTransitionMatrix(t *testing.T) {
	o, _ := testOptions().Normalize()
	states := []State{Queued, Running, Cancelling, RecoveryRequired, Completed, Failed, Cancelled}
	allowed := map[State]map[State]bool{
		Running:          {Running: true, RecoveryRequired: true, Completed: true, Failed: true},
		Cancelling:       {Cancelling: true, RecoveryRequired: true, Cancelled: true},
		RecoveryRequired: {RecoveryRequired: true, Failed: true, Cancelled: true},
	}
	for _, from := range states {
		for _, to := range states {
			t.Run(string(from)+"/"+string(to), func(t *testing.T) {
				j := Job{State: from, Progress: Preparing, Sequence: 2, Options: o,
					Provenance: Provenance{ModelSHA256: o.ModelSHA256, BuildDigest: o.BuildDigest}}
				u := Update{Sequence: 3, State: to}
				switch to {
				case Queued:
					u.Progress = Pending
				case Running:
					u.Progress = Measuring
				case Cancelling:
					u.Progress = Restoring
				case RecoveryRequired:
					u.Progress, u.Failure = Restoring, FailureRestoration
				case Completed:
					u = completeFor(j)
				case Failed:
					u.Progress, u.Restored, u.Failure = Done, true, FailureExecution
				case Cancelled:
					u.Progress, u.Restored = Done, true
				}
				err := validateUpdate(j, u)
				if (err == nil) != allowed[from][to] {
					t.Fatalf("transition permitted=%v: %v", allowed[from][to], err)
				}
			})
		}
	}
}

func TestSequenceReservesRestoration(t *testing.T) {
	o, _ := testOptions().Normalize()
	j := Job{State: Running, Progress: Measuring, Sequence: MaxSequence - 1, Options: o}
	u := Update{Sequence: MaxSequence, State: Running, Progress: Measuring}
	if err := validateUpdate(j, u); err != ErrInvalidUpdate {
		t.Fatal(err)
	}
	u.State, u.Progress, u.Restored, u.Failure = Failed, Done, true, FailureExecution
	if err := validateUpdate(j, u); err != nil {
		t.Fatal(err)
	}
}

func TestBoundsAndHistory(t *testing.T) {
	s, _ := openTest(t)
	var oldest string
	for i := range MaxHistory + 5 {
		j := startTest(t, s, "history")
		if i == 0 {
			oldest = j.ID
		}
		if _, err := s.Cancel("history", j.ID); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List("history")
	if err != nil || len(list) != MaxHistory {
		t.Fatalf("history = %d, %v", len(list), err)
	}
	if _, err := s.Get("history", oldest); err != ErrNotFound {
		t.Fatal("oldest history not evicted", err)
	}
	for i := range MaxJobs {
		startTest(t, s, fmt.Sprintf("worker-%d", i))
	}
	if len(s.jobs) != MaxJobs || len(s.active) != MaxJobs {
		t.Fatal("global capacity violated")
	}
	list, err = s.List("history")
	if err != nil || len(list) != 0 {
		t.Fatal("terminal history not reclaimed", err)
	}
	if _, err := s.Start("overflow", testOptions()); err != ErrCapacity {
		t.Fatal(err)
	}
	if _, err := s.Cancel("worker-0", s.active["worker-0"]); err != nil {
		t.Fatal(err)
	}
	startTest(t, s, "overflow")
	if len(s.jobs) != MaxJobs || len(s.active) != MaxJobs {
		t.Fatal("capacity after eviction")
	}
}

func TestConcurrentStartAndCancellation(t *testing.T) {
	s, _ := openTest(t)
	var wg sync.WaitGroup
	results := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			_, err := s.Start("worker", testOptions())
			results <- err
		})
	}
	wg.Wait()
	close(results)
	started := 0
	for err := range results {
		if err == nil {
			started++
		} else if err != ErrBusy {
			t.Fatal(err)
		}
	}
	if started != 1 {
		t.Fatalf("started %d", started)
	}
	a := leaseTest(t, s, "worker")
	results = make(chan error, 2)
	wg.Go(func() { _, err := s.Cancel("worker", a.Job.ID); results <- err })
	wg.Go(func() { _, err := s.Update("worker", a.Job.ID, a.Token, completeFor(a.Job)); results <- err })
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && err != ErrTransition {
			t.Fatal(err)
		}
	}
	j, err := s.Get("worker", a.Job.ID)
	if err != nil || (j.State != Completed && j.State != Cancelling) {
		t.Fatal("nonlinearized cancellation race", err)
	}
	fence(t, s, "worker", j.State == Cancelling)
}

func TestPoisonUncertainWrites(t *testing.T) {
	for _, written := range []bool{false, true} {
		t.Run(fmt.Sprintf("write-reached-disk=%v", written), func(t *testing.T) {
			s, dir := openTest(t)
			j := startTest(t, s, "worker")
			persist := s.persist
			s.persist = func(data []byte) error {
				if written {
					if err := persist(data); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("sensitive path and credential")
			}
			if _, err := s.Lease("worker"); err != ErrPoisoned {
				t.Fatal("failed lease did not poison", err)
			}
			if fenced, err := s.Fenced("unrelated"); !fenced || err != ErrPoisoned {
				t.Fatal("poison did not globally fail closed", fenced, err)
			}
			if _, err := s.Get("worker", j.ID); err != ErrPoisoned {
				t.Fatal(err)
			}
			if _, err := s.List("worker"); err != ErrPoisoned {
				t.Fatal(err)
			}
			if _, err := s.Start("other", testOptions()); err != ErrPoisoned {
				t.Fatal(err)
			}
			if _, err := s.Cancel("worker", j.ID); err != ErrPoisoned {
				t.Fatal(err)
			}
			if _, err := s.Update("worker", j.ID, "", Update{}); err != ErrPoisoned {
				t.Fatal(err)
			}
			_ = s.Close()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.Get("worker", j.ID)
			want := Queued
			if written {
				want = RecoveryRequired
			}
			if err != nil || got.State != want {
				t.Fatal("uncertain durable state not reconciled", err)
			}
			fence(t, s, "worker", true)
		})
	}
}

func TestPersistencePermissionsLockAndFailure(t *testing.T) {
	s, dir := openTest(t)
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, "benchmark"):                0700,
		filepath.Join(dir, "benchmark", stateName):     0600,
		filepath.Join(dir, "benchmark", "writer.lock"): 0600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("wrong persistence permissions", err)
		}
	}
	if other, err := Open(dir); err != ErrLocked || other != nil {
		t.Fatal("second writer allowed", err)
	}
	startTest(t, s, "worker")
	// Force an actual rename failure, not only a mocked persistence error.
	path := filepath.Join(dir, "benchmark", stateName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lease("worker"); err != ErrPoisoned {
		t.Fatal(err)
	}
	f, err := s.Fenced("worker")
	if !f || err != ErrPoisoned {
		t.Fatal("write failure did not fence", err)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "benchmark", ".jobs-*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("temporary write files leaked", err)
	}
	_ = s.Close()
	if _, err := Open(dir); err != ErrStorage {
		t.Fatal("invalid on-disk file accepted", err)
	}
	if fenced, err := s.Fenced("worker"); !fenced || err != ErrClosed {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEntropyFailureAndCollision(t *testing.T) {
	s, _ := openTest(t)
	s.random = bytes.NewReader(nil)
	if _, err := s.Start("worker", testOptions()); err != ErrStorage {
		t.Fatal(err)
	}
	fence(t, s, "worker", false)
	s.random = bytes.NewReader(bytes.Repeat([]byte{1}, 96))
	startTest(t, s, "worker")
	if _, err := s.Start("other", testOptions()); err != ErrStorage {
		t.Fatal("ID collision overwrote a job", err)
	}
	if len(s.jobs) != 1 {
		t.Fatal("collision changed job map")
	}
}
