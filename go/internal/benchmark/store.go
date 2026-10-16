// SPDX-License-Identifier: AGPL-3.0-or-later

package benchmark

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"maps"
	"os"
	"sort"
	"sync"
	"time"
)

type record struct {
	Job   Job    `json:"job"`
	Lease string `json:"lease"`
}

// Store serializes all operations and owns an exclusive on-disk writer lock.
// Use one store per coordinator data directory and Close it before reopening.
// A poisoned or closed store must never be used to authorize scheduler admission.
type Store struct {
	mu       sync.Mutex
	dir      string
	lock     *os.File
	jobs     map[string]record
	active   map[string]string
	poisoned bool
	closed   bool
	now      func() time.Time
	random   io.Reader
	persist  func([]byte) error
}

func (s *Store) ready() error {
	if s.closed {
		return ErrClosed
	}
	if s.poisoned {
		return ErrPoisoned
	}
	return nil
}

func (s *Store) index() {
	s.active = make(map[string]string)
	for id, r := range s.jobs {
		if !r.Job.State.Terminal() {
			s.active[r.Job.Machine] = id
		}
	}
}

// commit never publishes a candidate before durable persistence. ANY write
// failure poisons the store, including errors that might precede rename: callers
// cannot know whether a fence was durably acquired/released and must stop.
func (s *Store) commit(next map[string]record) error {
	trim(next)
	data, err := encode(next)
	if err != nil {
		s.poisoned = true
		return ErrPoisoned
	}
	if err := s.persist(data); err != nil {
		s.poisoned = true
		return ErrPoisoned
	}
	s.jobs = next
	s.index()
	return nil
}

func trim(jobs map[string]record) {
	var history []record
	for _, r := range jobs {
		if r.Job.State.Terminal() {
			history = append(history, r)
		}
	}
	sort.Slice(history, func(i, j int) bool {
		a, b := history[i].Job, history[j].Job
		if a.FinishedAt.Equal(b.FinishedAt) {
			return a.ID < b.ID
		}
		return a.FinishedAt.Before(b.FinishedAt)
	})
	remove := max(len(history)-MaxHistory, len(jobs)-MaxJobs)
	for i := 0; i < remove && i < len(history); i++ {
		delete(jobs, history[i].Job.ID)
	}
}

func timestamp(now time.Time, j Job) time.Time {
	now = now.UTC()
	if now.Before(j.UpdatedAt) {
		return j.UpdatedAt
	}
	return now
}

func expire(r record, now time.Time, restart bool) (record, bool) {
	j := &r.Job
	if j.State.Terminal() || j.State == RecoveryRequired || j.ApplyPhase == ApplyPrepared {
		return r, false
	}
	elapsed := !now.Before(deadline(*j))
	if !elapsed && !(restart && r.Lease != "") {
		return r, false
	}
	j.UpdatedAt = timestamp(now, *j)
	j.Failure = FailureDeadline
	if r.Lease == "" {
		j.State, j.Progress, j.FinishedAt = Failed, Done, j.UpdatedAt
	} else {
		j.State, j.Progress = RecoveryRequired, Restoring
		if !elapsed {
			j.Failure = FailureRestart
		}
	}
	return r, true
}

// Expiry is lazy per machine (and comprehensive at Open). No timer is needed
// for safety: all outstanding states already fence admission. Lookup is O(1)
// without a persistence operation unless the accessed machine just expired.
func (s *Store) expireMachine(machine string, now time.Time) error {
	id, ok := s.active[machine]
	if !ok {
		return nil
	}
	r, changed := expire(s.jobs[id], now, false)
	if !changed {
		return nil
	}
	next := maps.Clone(s.jobs)
	next[id] = r
	return s.commit(next)
}

func (s *Store) prepare(machine string) error {
	return s.prepareAt(machine, s.now())
}

func (s *Store) prepareAt(machine string, now time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !validMachine(machine) {
		return ErrInvalidMachine
	}
	return s.expireMachine(machine, now)
}

func (s *Store) token() (string, error) {
	var b [32]byte
	if _, err := io.ReadFull(s.random, b[:]); err != nil {
		return "", ErrStorage
	}
	return hex.EncodeToString(b[:]), nil
}

// Start reserves/fences machine and durably queues a job. Numeric zero options
// select defaults; both digests must already be pinned by trusted discovery.
// Queued, running, cancelling and recovery jobs all count as outstanding.
func (s *Store) Start(machine string, options Options) (Job, error) {
	return s.StartWithProvenance(machine, options, Audit{})
}

// StartWithProvenance pins trusted audit values in the same durable transaction
// as the job and fence. Empty values remain explicitly unknown for old callers.
func (s *Store) StartWithProvenance(machine string, options Options, audit Audit) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepare(machine); err != nil {
		return Job{}, err
	}
	if !audit.valid() {
		return Job{}, ErrInvalidOptions
	}
	o, err := options.Normalize()
	if err != nil {
		return Job{}, err
	}
	if _, ok := s.active[machine]; ok {
		return Job{}, ErrBusy
	}
	if len(s.active) >= MaxJobs {
		return Job{}, ErrCapacity
	}
	id, err := s.token()
	if err != nil {
		return Job{}, err
	}
	// A collision is never allowed to replace a job, even with a broken RNG.
	if _, exists := s.jobs[id]; exists {
		return Job{}, ErrStorage
	}
	now := s.now().UTC()
	j := Job{
		ID: id, Machine: machine, Options: o, State: Queued, Progress: Pending,
		UpdatedAt: now,
		Provenance: Provenance{
			Audit:         audit,
			SchemaVersion: SchemaVersion, PolicyVersion: PolicyVersion, FixtureVersion: FixtureVersion,
			WorkerID: machine, CreatedAt: now, DeadlineAt: now.Add(time.Duration(o.BudgetSeconds) * time.Second),
			ModelSHA256: o.ModelSHA256, BuildDigest: o.BuildDigest,
		},
	}
	next := maps.Clone(s.jobs)
	next[id] = record{Job: j}
	if err := s.commit(next); err != nil {
		return Job{}, err
	}
	return snapshot(j), nil
}

func (s *Store) lookup(machine, id string) (record, error) {
	if !validHex(id, 64) {
		return record{}, ErrNotFound
	}
	r, ok := s.jobs[id]
	if !ok || r.Job.Machine != machine {
		return record{}, ErrNotFound
	}
	return r, nil
}

// Get returns a detached snapshot, scoped to machine. A cross-machine ID is
// indistinguishable from an unknown ID. It never returns a lease token.
func (s *Store) Get(machine, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepare(machine); err != nil {
		return Job{}, err
	}
	r, err := s.lookup(machine, id)
	if err != nil {
		return Job{}, err
	}
	return snapshot(r.Job), nil
}

// List returns only machine's retained jobs, newest first (ID breaks ties).
// The result is bounded by MaxJobs and contains no lease tokens.
func (s *Store) List(machine string) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepare(machine); err != nil {
		return nil, err
	}
	out := make([]Job, 0)
	for _, r := range s.jobs {
		if r.Job.Machine == machine {
			out = append(out, snapshot(r.Job))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provenance.CreatedAt.Equal(out[j].Provenance.CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].Provenance.CreatedAt.After(out[j].Provenance.CreatedAt)
	})
	return out, nil
}

// Lease durably transitions queued -> running and returns a random token. A
// running job is not re-leased, except prepared Apply. Prepared Apply and
// cancelling/recovery jobs return their EXISTING token with RecoveryOnly=true,
// allowing authenticated durable decision reconciliation/restoration after a lost
// response or worker restart. Such an assignment must NEVER execute a benchmark.
func (s *Store) Lease(machine string) (Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err := s.prepareAt(machine, now); err != nil {
		return Assignment{}, err
	}
	id, ok := s.active[machine]
	if !ok {
		return Assignment{}, ErrNoWork
	}
	r := s.jobs[id]
	if r.Job.State == RecoveryRequired || r.Job.State == Cancelling || r.Job.ApplyPhase == ApplyPrepared {
		return Assignment{Action: r.Job.Action, Job: snapshot(r.Job), Token: r.Lease, RecoveryOnly: true}, nil
	}
	if r.Job.State != Queued {
		return Assignment{}, ErrNoWork
	}
	token, err := s.token()
	if err != nil {
		return Assignment{}, err
	}
	r.Lease = token
	r.Job.State, r.Job.Progress = Running, Preparing
	r.Job.UpdatedAt = timestamp(now, r.Job)
	next := maps.Clone(s.jobs)
	next[id] = r
	if err := s.commit(next); err != nil {
		return Assignment{}, err
	}
	return Assignment{Action: r.Job.Action, Job: snapshot(r.Job), Token: token}, nil
}

func validateUpdate(j Job, u Update) error {
	if j.Action == ActionApply {
		return validateApplyUpdate(j, u)
	}
	if u.Action != "" || u.Result != "" {
		return ErrInvalidUpdate
	}
	if j.State.Terminal() || j.State == Queued {
		return ErrTransition
	}
	if u.Sequence == 0 || u.Sequence > MaxSequence || u.Sequence != j.Sequence+1 {
		return ErrReplay
	}
	if u.State != Completed && u.Summary != nil {
		return ErrInvalidUpdate
	}
	if u.State.Terminal() {
		if !u.Restored || u.Progress != Done {
			return ErrInvalidUpdate
		}
	} else if u.Restored {
		return ErrInvalidUpdate
	}
	switch u.State {
	case Running:
		if j.State != Running {
			return ErrTransition
		}
		// Reserve the last sequence for restoration acknowledgement.
		if u.Sequence == MaxSequence || stage(u.Progress) < stage(j.Progress) || stage(u.Progress) < 1 || stage(u.Progress) > 6 || u.Failure != NoFailure {
			return ErrInvalidUpdate
		}
	case Cancelling:
		if j.State != Cancelling {
			return ErrTransition
		}
		if u.Sequence == MaxSequence || u.Progress != Restoring || u.Failure != NoFailure {
			return ErrInvalidUpdate
		}
	case RecoveryRequired:
		if u.Sequence == MaxSequence || u.Progress != Restoring || !validFailure(u.Failure) {
			return ErrInvalidUpdate
		}
	case Completed:
		if j.State != Running {
			return ErrTransition
		}
		if u.Failure != NoFailure || u.Summary == nil || !u.Summary.valid(j.Options) {
			return ErrInvalidUpdate
		}
	case Failed:
		if j.State == Cancelling {
			return ErrTransition
		}
		if !validFailure(u.Failure) {
			return ErrInvalidUpdate
		}
	case Cancelled:
		if j.State != Cancelling && j.State != RecoveryRequired {
			return ErrTransition
		}
		if u.Failure != NoFailure {
			return ErrInvalidUpdate
		}
	default:
		return ErrInvalidUpdate
	}
	return nil
}

// Update accepts a scoped, leased, strictly sequenced worker acknowledgement.
// The caller MUST authenticate machine independently of the lease token and
// authorize restoration acknowledgement only after job-owned processes stop and
// prior service/configuration/health are verified restored. Tokens are not auth.
func (s *Store) Update(machine, id, lease string, update Update) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err := s.prepareAt(machine, now); err != nil {
		return Job{}, err
	}
	r, err := s.lookup(machine, id)
	if err != nil {
		return Job{}, err
	}
	if !validHex(lease, 64) || r.Lease == "" || subtle.ConstantTimeCompare([]byte(lease), []byte(r.Lease)) != 1 {
		return Job{}, ErrLease
	}
	if err := validateUpdate(r.Job, update); err != nil {
		return Job{}, err
	}
	if update.State == Completed && !timestamp(now, r.Job).Before(deadline(r.Job)) {
		return Job{}, ErrTransition
	}
	r.Job.State, r.Job.Progress = update.State, update.Progress
	r.Job.Sequence, r.Job.Failure, r.Job.Restored = update.Sequence, update.Failure, update.Restored
	r.Job.Result = update.Result
	if r.Job.Action != ActionApply {
		r.Job.Summary = nil
	}
	if update.Summary != nil {
		summary := *update.Summary
		r.Job.Summary = &summary
	}
	r.Job.UpdatedAt = timestamp(now, r.Job)
	if r.Job.State.Terminal() {
		r.Job.FinishedAt = r.Job.UpdatedAt
	}
	next := maps.Clone(s.jobs)
	next[id] = r
	if err := s.commit(next); err != nil {
		return Job{}, err
	}
	return snapshot(r.Job), nil
}

// Cancel is idempotent. Unleased queued work cancels immediately; leased work
// remains fenced. Recovery-required work cannot be demoted out of recovery.
// Prepared Apply is an irrevocable authorization and remains unchanged/fenced.
// Terminal jobs are returned unchanged, never rewritten as cancelled.
func (s *Store) Cancel(machine, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepare(machine); err != nil {
		return Job{}, err
	}
	r, err := s.lookup(machine, id)
	if err != nil {
		return Job{}, err
	}
	if r.Job.State.Terminal() || r.Job.State == Cancelling || r.Job.State == RecoveryRequired || r.Job.ApplyPhase == ApplyPrepared {
		return snapshot(r.Job), nil
	}
	r.Job.UpdatedAt = timestamp(s.now(), r.Job)
	if r.Job.State == Queued && r.Lease == "" {
		r.Job.State, r.Job.Progress, r.Job.FinishedAt = Cancelled, Done, r.Job.UpdatedAt
	} else {
		r.Job.State, r.Job.Progress = Cancelling, Restoring
	}
	next := maps.Clone(s.jobs)
	next[id] = r
	if err := s.commit(next); err != nil {
		return Job{}, err
	}
	return snapshot(r.Job), nil
}

// Fenced is true for EVERY outstanding state, including queued. On any error it
// returns true: callers MUST NOT treat an error as permission to schedule. This
// check alone is not an atomic scheduler drain/admission transaction.
func (s *Store) Fenced(machine string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepare(machine); err != nil {
		return true, err
	}
	_, ok := s.active[machine]
	return ok, nil
}

// Apply is deliberately unsupported and never changes state or configuration.
func (s *Store) Apply(machine, id string) error { return ErrUnsupported }

// Close releases the exclusive writer lock. It is idempotent. The closed store
// fails all operations (except Apply and Close) closed, including fence checks.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.lock.Close(); err != nil {
		return ErrStorage
	}
	return nil
}

func newStore(dir string, lock *os.File) *Store {
	s := &Store{dir: dir, lock: lock, jobs: make(map[string]record), active: make(map[string]string), now: time.Now, random: rand.Reader}
	s.persist = s.writeAtomic
	return s
}
