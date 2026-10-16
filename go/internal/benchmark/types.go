// SPDX-License-Identifier: AGPL-3.0-or-later

// Package benchmark provides a bounded, durable, single-writer benchmark job
// store. It does not authenticate workers, execute benchmarks, or apply settings.
// See README.md for the coordinator/worker security contract.
package benchmark

import (
	"errors"
	"math"
	"time"
)

const (
	DefaultContext       = 32768
	DefaultParallel      = 2
	DefaultBudgetSeconds = 600
	MinContext           = 8192
	MaxContext           = 131072
	MaxParallel          = 8
	MinBudgetSeconds     = 30
	MaxBudgetSeconds     = 1800
	MaxJobs              = 256 // Includes outstanding jobs and retained history.
	MaxHistory           = 128 // Global, not per machine.
	MaxSequence          = 10000
	SchemaVersion        = 1
	PolicyVersion        = 1
	FixtureVersion       = 1
)

// Errors deliberately contain no worker input, paths, credentials, or OS errors.
var (
	ErrInvalidOptions = errors.New("benchmark: invalid options")
	ErrInvalidMachine = errors.New("benchmark: invalid machine")
	ErrNotFound       = errors.New("benchmark: job not found")
	ErrBusy           = errors.New("benchmark: machine has outstanding work")
	ErrCapacity       = errors.New("benchmark: capacity reached")
	ErrNoWork         = errors.New("benchmark: no work available")
	ErrLease          = errors.New("benchmark: invalid lease")
	ErrInvalidUpdate  = errors.New("benchmark: invalid update")
	ErrTransition     = errors.New("benchmark: invalid transition")
	ErrReplay         = errors.New("benchmark: invalid update sequence")
	ErrStorage        = errors.New("benchmark: storage unavailable")
	ErrCorrupt        = errors.New("benchmark: invalid stored state")
	ErrPoisoned       = errors.New("benchmark: store requires reopen")
	ErrLocked         = errors.New("benchmark: store already open")
	ErrClosed         = errors.New("benchmark: store closed")
	ErrUnsupported    = errors.New("benchmark: apply unsupported")
)

// Options is the complete input allowlist. Zero numeric values select automatic
// defaults. ModelSHA256 and BuildDigest are REQUIRED pinned, lowercase SHA-256
// hex digests, not paths, model names, URLs, or executable commands.
type Options struct {
	Context       int    `json:"context"`
	Parallel      int    `json:"parallel"`
	BudgetSeconds int    `json:"budget_seconds"`
	ModelSHA256   string `json:"model_sha256"`
	BuildDigest   string `json:"build_digest"`
}

// Normalize applies numeric defaults and validates all fields. Context must be
// a multiple of 1024. Digests must be nonzero, 64-character lowercase hex.
func (o Options) Normalize() (Options, error) {
	if o.Context == 0 {
		o.Context = DefaultContext
	}
	if o.Parallel == 0 {
		o.Parallel = DefaultParallel
	}
	if o.BudgetSeconds == 0 {
		o.BudgetSeconds = DefaultBudgetSeconds
	}
	if o.Context < MinContext || o.Context > MaxContext || o.Context%1024 != 0 ||
		o.Parallel < 1 || o.Parallel > MaxParallel ||
		o.BudgetSeconds < MinBudgetSeconds || o.BudgetSeconds > MaxBudgetSeconds ||
		!validDigest(o.ModelSHA256) || !validDigest(o.BuildDigest) {
		return Options{}, ErrInvalidOptions
	}
	return o, nil
}

// State is the persisted lifecycle. Unknown values are never accepted.
type State string

const (
	Queued           State = "queued"
	Running          State = "running"
	Cancelling       State = "cancelling"
	Completed        State = "completed"
	Failed           State = "failed"
	Cancelled        State = "cancelled"
	RecoveryRequired State = "recovery_required"
)

// Terminal reports whether a state releases the machine fence.
func (s State) Terminal() bool { return s == Completed || s == Failed || s == Cancelled }

// Progress is a fixed stage, not worker-supplied log text. Running stages cannot
// move backwards. Recovery and cancellation permit only Restoring.
type Progress string

const (
	Pending    Progress = "pending"
	Preparing  Progress = "preparing"
	Loading    Progress = "loading"
	Warming    Progress = "warming"
	Measuring  Progress = "measuring"
	Validating Progress = "validating"
	Restoring  Progress = "restoring"
	Done       Progress = "done"
)

func stage(p Progress) int {
	switch p {
	case Pending:
		return 0
	case Preparing:
		return 1
	case Loading:
		return 2
	case Warming:
		return 3
	case Measuring:
		return 4
	case Validating:
		return 5
	case Restoring:
		return 6
	case Done:
		return 7
	}
	return -1
}

// Failure is a sanitized reason code; there is intentionally no error-text field.
type Failure string

const (
	NoFailure          Failure = ""
	FailureDeadline    Failure = "deadline"
	FailureRestart     Failure = "restart"
	FailureExecution   Failure = "execution"
	FailureQuality     Failure = "quality"
	FailureModel       Failure = "model_mismatch"
	FailureRestoration Failure = "restoration"
)

func validFailure(f Failure) bool {
	switch f {
	case FailureDeadline, FailureRestart, FailureExecution, FailureQuality, FailureModel, FailureRestoration:
		return true
	}
	return false
}

// Quality is the fixed fixture-quality verdict. Only QualityPass can complete.
type Quality string

const (
	QualityPass Quality = "pass"
	QualityFail Quality = "fail"
)

// Summary is evidence attested by the authenticated worker, NOT independently
// measured by this store. Context and Parallel must equal the pinned request.
// Counts describe one measured long, non-warmup row, never synthesized padding.
// DurationSeconds is decode time (Python decode_s), not prefill or wall time.
// Throughput must agree with OutputTokens / DurationSeconds within 1%.
// ModelVerified attests hashing the actual loaded artifact, not trusting its name.
type Summary struct {
	Quality         Quality `json:"quality"`
	Context         int     `json:"context"`
	Parallel        int     `json:"parallel"`
	PromptTokens    int     `json:"prompt_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	OutputAllowance int     `json:"output_allowance,omitempty"`
	ReservedTokens  int     `json:"reserved_tokens,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	TokensPerSecond float64 `json:"tokens_per_second"`
	ModelVerified   bool    `json:"model_verified"`
	ModelSHA256     string  `json:"model_sha256"`
	BuildDigest     string  `json:"build_digest"`
}

func (s Summary) valid(o Options) bool {
	if s.Quality != QualityPass || !s.ModelVerified || s.ModelSHA256 != o.ModelSHA256 ||
		s.BuildDigest != o.BuildDigest || s.Context != o.Context || s.Parallel != o.Parallel ||
		s.PromptTokens < 1 || s.PromptTokens >= o.Context || s.OutputTokens < 1 ||
		s.OutputTokens > o.Context || !s.validContext() ||
		!finitePositive(s.DurationSeconds, float64(o.BudgetSeconds)) ||
		!finitePositive(s.TokensPerSecond, 1_000_000) {
		return false
	}
	want := float64(s.OutputTokens) / s.DurationSeconds
	return finitePositive(want, 1_000_000) && math.Abs(s.TokensPerSecond-want) <= want*0.01
}

// Zero allowance/reserve preserves the original exact-full-context evidence.
// New evidence budgets the requested output separately from measured output.
func (s Summary) validContext() bool {
	if s.OutputAllowance == 0 && s.ReservedTokens == 0 {
		return s.PromptTokens+s.OutputTokens == s.Context
	}
	if s.OutputAllowance < s.OutputTokens || s.OutputAllowance > s.Context ||
		s.ReservedTokens < 0 || s.ReservedTokens > 104 {
		return false
	}
	// Subtraction after individual bounds avoids integer overflow. Python's
	// fixture permits 96 tokens of packing slack and reserves 8 tokens.
	remaining := s.Context - s.PromptTokens - s.OutputAllowance
	return remaining >= s.ReservedTokens && remaining <= 104 && remaining-s.ReservedTokens <= 96
}

func finitePositive(n, max float64) bool {
	return !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 && n <= max
}

// Audit identifies the authenticated role and trusted startup binding. Empty
// fields mean unknown (legacy records), never inferred from current settings.
// Actor is a nonsecret role/opaque identifier, not a credential or HTTP header.
type Audit struct {
	Actor            string `json:"actor,omitempty"`
	CodeRevision     string `json:"code_revision,omitempty"`
	ConfigGeneration string `json:"config_generation,omitempty"`
}

func (a Audit) valid() bool {
	if a.Actor != "" && !validMachine(a.Actor) {
		return false
	}
	if a.CodeRevision != "" && !(validHex(a.CodeRevision, 40) || validHex(a.CodeRevision, 64)) {
		return false
	}
	return a.ConfigGeneration == "" || validDigest(a.ConfigGeneration)
}

// Provenance is pinned by Start, copied into every snapshot, and never updated.
// Numeric versions describe this package's contract, not worker software.
// Optional audit fields on legacy records remain unknown.
type Provenance struct {
	Audit
	SchemaVersion  int       `json:"schema_version"`
	PolicyVersion  int       `json:"policy_version"`
	FixtureVersion int       `json:"fixture_version"`
	WorkerID       string    `json:"worker_id"`
	CreatedAt      time.Time `json:"created_at"`
	DeadlineAt     time.Time `json:"deadline_at"`
	ModelSHA256    string    `json:"model_sha256"`
	BuildDigest    string    `json:"build_digest"`
}

// Job is a detached snapshot with no lease secret. Restored records an explicit
// worker acknowledgement; it is false for unleased cancellations/expiry.
type Action string

const ActionApply Action = "apply"

// ApplyResult is a worker attestation of verified service/configuration health.
type ApplyResult string

const (
	Applied    ApplyResult = "applied"
	RolledBack ApplyResult = "rolled_back"
)

// ApplyRequest retains completed benchmark evidence and the explicit admin audit.
type ApplyRequest struct {
	TransactionID     string    `json:"transaction_id"`
	Generation        uint64    `json:"generation"`
	Audit             Audit     `json:"audit"`
	RequestedAt       time.Time `json:"requested_at"`
	DeadlineAt        time.Time `json:"deadline_at"`
	CompletedAt       time.Time `json:"completed_at"`
	CompletedSequence uint64    `json:"completed_sequence"`
}

// ApplyPhase is a durable distributed decision, distinct from execution progress.
type ApplyPhase string

const (
	ApplyPrepared   ApplyPhase = "prepared"
	ApplyApplied    ApplyPhase = "applied"
	ApplyRolledBack ApplyPhase = "rolled_back"
)

// Reconciliation attests an fsynced local decision while local admission is fenced.
// Applied and rolled_back are irreversible; HTTP uncertainty must not undo them.
type Reconciliation struct {
	TransactionID string     `json:"transaction_id"`
	Generation    uint64     `json:"generation"`
	Phase         ApplyPhase `json:"phase"`
	Durable       bool       `json:"durable"`
}

type Job struct {
	ApplyPhase   ApplyPhase    `json:"apply_phase,omitempty"`
	Action       Action        `json:"action,omitempty"`
	Result       ApplyResult   `json:"result,omitempty"`
	ApplyRequest *ApplyRequest `json:"apply_request,omitempty"`
	ID           string        `json:"id"`
	Machine      string        `json:"machine"`
	Options      Options       `json:"options"`
	Provenance   Provenance    `json:"provenance"`
	State        State         `json:"state"`
	Progress     Progress      `json:"progress"`
	Sequence     uint64        `json:"sequence"`
	Failure      Failure       `json:"failure"`
	Restored     bool          `json:"restored"`
	Summary      *Summary      `json:"summary,omitempty"`
	UpdatedAt    time.Time     `json:"updated_at"`
	FinishedAt   time.Time     `json:"finished_at,omitzero"`
}

// Assignment is returned ONLY to the authenticated worker for Machine. Never
// expose Token through admin/public snapshots or log it. RecoveryOnly prohibits
// starting/restarting a benchmark or Apply. Ordinarily restore original config;
// prepared Apply instead reconciles its durable local decision (see README).
type Assignment struct {
	Action       Action `json:"action,omitempty"`
	Job          Job    `json:"job"`
	Token        string `json:"lease"`
	RecoveryOnly bool   `json:"recovery_only"`
}

// Update is the entire worker-update allowlist. Sequence must be exactly the
// last accepted sequence + 1. Benchmark terminal updates require Restored=true;
// benchmark completion requires Summary, other states forbid it. Apply updates
// require ActionApply and forbid Summary. Terminal Apply decisions must instead
// use ReconcileApply; prepared Apply refuses further progress updates.
// No provenance or retained benchmark evidence is mutable.
type Update struct {
	Action   Action      `json:"action,omitempty"`
	Result   ApplyResult `json:"result,omitempty"`
	Sequence uint64      `json:"sequence"`
	State    State       `json:"state"`
	Progress Progress    `json:"progress"`
	Failure  Failure     `json:"failure"`
	Restored bool        `json:"restored"`
	Summary  *Summary    `json:"summary,omitempty"`
}

func validMachine(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (i > 0 && c == '-')) {
			return false
		}
	}
	return true
}

func validHex(s string, size int) bool {
	if len(s) != size {
		return false
	}
	for i := range len(s) {
		if !((s[i] >= '0' && s[i] <= '9') || (s[i] >= 'a' && s[i] <= 'f')) {
			return false
		}
	}
	return true
}

func validDigest(s string) bool {
	if !validHex(s, 64) {
		return false
	}
	for i := range len(s) {
		if s[i] != '0' {
			return true
		}
	}
	return false
}

func snapshot(j Job) Job {
	if j.ApplyRequest != nil {
		a := *j.ApplyRequest
		j.ApplyRequest = &a
	}
	if j.Summary != nil {
		s := *j.Summary
		j.Summary = &s
	}
	return j
}
