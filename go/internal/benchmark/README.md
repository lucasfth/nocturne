# Durable benchmark jobs (Go helper only)

This package is coordinator storage, **not** a dispatcher, worker executor,
benchmark implementation, scheduler drain, or admin API. It stores the explicit
Apply control-plane workflow described below. It has
no network calls, subprocesses, model downloads, or third-party dependencies.
Python remains the default gateway. No runtime/UI parity is claimed.

## Exact API

```go
func Open(dataDir string) (*Store, error)

func (s *Store) Start(machine string, options Options) (Job, error)
func (s *Store) StartWithProvenance(machine string, options Options, audit Audit) (Job, error)
func (s *Store) Get(machine, id string) (Job, error)
func (s *Store) List(machine string) ([]Job, error)
func (s *Store) Lease(machine string) (Assignment, error)
func (s *Store) Update(machine, id, lease string, update Update) (Job, error)
func (s *Store) Cancel(machine, id string) (Job, error)
func (s *Store) Fenced(machine string) (bool, error)
func (s *Store) Apply(machine, id string) error // legacy: always ErrUnsupported
func (s *Store) RequestApply(machine, id string, target ApplyTarget) (Job, error)
func (s *Store) ReconcileApply(machine, id, lease string, update Reconciliation) (Job, error)
func (s *Store) Close() error

func (o Options) Normalize() (Options, error)
func (s State) Terminal() bool
```

Use `Open`, not the zero value of `Store`. All store operations are serialized.
Returned jobs/lists/summaries are detached snapshots. `Get`, `List`, `Start`,
`Cancel`, and `Update` never return lease secrets. Cross-machine job IDs return
`ErrNotFound`, just like unknown/malformed IDs. Machine IDs follow the registry's
`^[a-z0-9][a-z0-9-]{0,31}$` syntax; existence/authorization is the caller's job.
`List` is newest-created first, with lexical ID tie-breaking.

### Inputs and evidence

```go
type Options struct {
    Context, Parallel, BudgetSeconds int
    ModelSHA256, BuildDigest         string
}

type Assignment struct {
    Job          Job
    Token        string
    RecoveryOnly bool
}

type Update struct {
    Sequence uint64
    State    State
    Progress Progress
    Failure  Failure
    Restored bool
    Summary  *Summary
}

type Summary struct {
    Quality                          Quality
    Context, Parallel                int
    PromptTokens, OutputTokens       int
    OutputAllowance, ReservedTokens  int
    DurationSeconds, TokensPerSecond float64
    ModelVerified                    bool
    ModelSHA256, BuildDigest          string
}
```

Numeric zero options resolve automatically to **Context=32768, Parallel=2,
BudgetSeconds=600**. Bounds are:

- Context: 8192–131072, multiples of 1024.
- Parallel: 1–8.
- BudgetSeconds: 30–1800; deadline is fixed at creation, not lease time.
- Both digests: required nonzero, lowercase, 64-character SHA-256 hex. Build
  digest identifies the executor/backend build; it is not an executable path.

There is no command, path, URL, free-form model name, log text, or arbitrary JSON
configuration field. Trusted discovery must supply both hashes before `Start`;
`Options{}` alone is intentionally insufficient. Automatic candidate discovery is
**not implemented**. The lead's route layer should resolve trusted digests rather
than exposing required catalog/hash editing to the normal admin UI.

`Job` contains ID, Machine, normalized Options, immutable Provenance, State,
Progress, Sequence, Failure, Restored, optional Summary, UpdatedAt and FinishedAt.
`Provenance` contains SchemaVersion, PolicyVersion, FixtureVersion (all currently
1), WorkerID, CreatedAt, DeadlineAt, ModelSHA256, and BuildDigest. Versions describe
this helper's pinned contract; workers must implement the matching fixture/policy.
These fields cannot be changed by any update. Optional `actor`, `code_revision`
and `config_generation` are pinned by `StartWithProvenance` in the job/fence
transaction. Actor is a nonsecret lowercase role/opaque ID (machine-ID syntax).
The HTTP adapter records `admin-bearer`, the authenticated fleet-wide role, NOT a
person inferred from request headers. Trusted `BenchmarkTarget` supplies code
revision (40/64 lowercase hex) and configuration generation (nonzero SHA-256
identity of the worker configuration generation). Never supply credentials,
commands, paths or raw configuration. These fields are not browser inputs.
Generation must change whenever applicable worker configuration changes; its
identity is the adapter's responsibility, not an inferred coordinator counter.

The additive disk schema remains version 1: absent audit fields on old records
and old `Start` callers mean **unknown**, are omitted in JSON and never backfilled
from current settings. Optional startup adapter values preserve old disposable
adapters; missing provenance is not suitable evidence for future Apply. Unknown
keys and malformed present values still fail closed. Older binaries cannot read
new fields; forward read compatibility is not claimed. There is no signature or
remote attestation; integration must persist restoration/authorization data.

Only `QualityPass` can complete. Summary requires `ModelVerified=true`, exact
pinned model/build digests, exact requested context/parallel, positive prompt and
output tokens, finite positive decode duration no greater than the budget, and
finite positive throughput no greater than 1,000,000 tokens/s. Throughput must
match actual output tokens / decode duration within 1%.

For Python `worker/model_benchmark.py`, select one **long, non-warmup** measured
row: `prompt_tokens` -> PromptTokens, `decode_tokens` -> OutputTokens,
`decode_s` -> DurationSeconds, `decode_tps` -> TokensPerSecond. Do not use
`wall_s`, `prefill_s`, aggregate parallel throughput, or `occupied_context` as a
measured token count. OutputAllowance is the requested `c['tokens']`; ReservedTokens
is the actual safety margin (currently 8). These are optional JSON fields
`output_allowance` and `reserved_tokens`, not browser options. Validation requires:

- actual output <= output allowance <= context;
- prompt + output allowance + reserved tokens <= context;
- remaining packing slack after that reserve <= 96 tokens;
- total unoccupied budget before reserve <= 104 (96 + 8) tokens.

Thus prompt + actual output + actual reserve also fits. No rounding, padding or
invented counts is permitted. Python currently verifies decode count equals the
allowance; the contract can also represent honest early termination without
inflating output. With both new fields zero/absent, the original exact
prompt+output=context rule is retained for old evidence. Python's backend version
string is **not** BuildDigest: a trusted adapter must independently verify the
artifact/build digests and quality verdict before submitting a summary. NaN, infinity, bogus enums, missing evidence and inconsistent values
are rejected without changing the job. This is a deliberately narrow summary,
not aggregate multi-trial statistics or arbitrary worker output.

**These are authenticated worker attestations, not independent measurement by
this package.** The integration must actually hash the loaded model, verify the
build, run the versioned quality fixture/context/concurrency tests, and verify
restoration. This package never synthesizes successful results.

## Lifecycle and fences

The following lifecycle/sequence rules describe ordinary benchmarks. Apply's
prepared/terminal reconciliation exceptions are specified separately below.

| Operation/event | Result | Fence |
| --- | --- | --- |
| `Start` | queued / pending | retained |
| `Lease` on queued | running / preparing, new 256-bit random token | retained |
| `Cancel` on queued | cancelled / done, never executed | released |
| Queued deadline | failed / deadline, never executed | released |
| `Cancel` on running | cancelling / restoring | retained |
| Restart or deadline of leased work | recovery_required / restoring | retained |
| Worker reports restoration problem | recovery_required / restoring | retained |
| Valid running completion + restored acknowledgement | completed / done | released |
| Running/recovery failure + restored acknowledgement | failed / done | released |
| Cancelling/recovery cancellation + restored acknowledgement | cancelled / done | released |

A running job is not re-leased (`ErrNoWork`). `Lease` on cancelling/recovery work
returns the **existing** token with `RecoveryOnly=true`; this must **never** launch
a benchmark. It allows an authenticated worker to recover after losing a lease
response or restarting. Lost running lease responses require cancellation,
deadline expiry, or coordinator restart before restoration-only re-delivery.

Recovery cannot transition back to running or completed. Cancellation cannot
transition to completed or failed: acknowledge cancelled after restoration, or
report recovery_required if restoration fails. `Cancel` is idempotent, leaves
terminal jobs unchanged, and never clears a leased/recovery fence by itself.
There is no force-unfence/delete/retry/requeue API.

Worker updates must use exactly the last accepted `Sequence + 1`, starting at 1.
Replay, gaps and terminal replays fail. The cap is 10000; nonterminal updates must
leave sequence 10000 available for a final restored acknowledgement. Coordinator
cancellation, expiry and restart do not consume worker sequence numbers. A lost
update response can be reconciled using `Get`/the worker's scoped status route.

Progress is a fixed enum: pending, preparing, loading, warming, measuring,
validating, restoring, done. Running stages cannot go backwards; cancelling and
recovery permit only restoring. Intermediate progress reports are optional.
Failure codes are empty (no failure), deadline, restart, execution, quality,
model_mismatch and restoration. There are no worker error messages in storage.

Expiry is checked on access to the affected machine, and for every job at Open;
there is no background timer. This is safe for admission because **every
outstanding state already fences**. Workers still need independent deadlines,
process termination and restoration watchdogs. Wall-clock deadlines are not
extended by progress; timestamp ordering is clamped against clock rollback.

## Mandatory integration contract

1. Authenticate/authorize every machine-scoped operation using existing gateway
   security. Admin requests still need Bearer authentication and origin checks.
   Worker identity must come from authenticated credentials, not request fields.
   A lease is a job-bound capability, **not worker authentication**. Route decoders
   must reject unknown/duplicate fields and enforce request limits; Go structs
   alone do not make permissive HTTP JSON decoding strict.
2. Atomically coordinate scheduler admission with `Start`/`Fenced`, then drain
   in-flight work before execution. `Fenced` alone is not a scheduler lock and
   cannot eliminate a check-then-dispatch race in another component.
3. Deliver `Assignment.Token` only to that authenticated machine. Never log it or
   expose it in admin/public responses. Persist the worker's prior configuration
   and job-owned process identities before making changes. Recovery must address
   the same job even when its original token response was lost.
4. Treat `RecoveryOnly` as restoration-only. Send terminal `Restored=true` only
   after job-owned processes have stopped and the prior configuration, service
   and health have been verified restored. A boolean assertion from an arbitrary
   HTTP caller is not sufficient. Recheck machine authorization at dispatch.
5. **Any `Open` failure disables admission/dispatch. Any operation error must
   never grant admission.** `Fenced` returns `(true, err)` on failure. Do not
   replace corrupt/poisoned storage with a fresh empty store or ignore errors.
6. Completion is not apply. Only explicit `RequestApply` can enqueue Apply;
   the legacy unbound `Apply` remains unsupported. See the exact contract below.

## Explicit Apply wire contract (opt-in adapters only)

`POST /admin/machines/{machine}/benchmarks/{id}/apply` accepts **only `{}`**.
Existing admin Bearer authentication, browser-origin checks, limits, machine
scoping and scheduler locking apply. Success returns 202 with the updated Job;
unsupported, stale, non-completed, unrestored, busy or repeated Apply returns
409. Unknown jobs return 404. No browser-supplied configuration, paths, commands,
generation, model or build are accepted.

The trusted startup `BenchmarkTarget.ApplyEnabled` must explicitly be true.
Production Apply remains disabled. Benchmark execution requires explicit verified installation and protected startup targets. `RequestApply` requires nonempty valid
actor/revision/generation audit fields and exact matching of the completed job's
configuration generation, code revision, model SHA-256 and build digest. The
HTTP lease route rechecks this binding before dispatch. The worker must also
compare the **actual local** generation/artifacts/revision before mutation;
startup discovery is not proof that local configuration has not changed.

Apply reuses the job ID, atomically fences the machine, and consumes the job's
single Apply opportunity. It preserves `options`, `provenance`, and `summary`.
New optional Job fields (absent for ordinary/legacy benchmarks):

- `action: "apply"`
- `apply_request: {transaction_id, generation, audit: {actor, code_revision,
  config_generation}, requested_at, deadline_at, completed_at, completed_sequence}`.
  Transaction ID is a fresh random 256-bit lowercase hex identity. Generation is
  1 (one Apply opportunity per job), separate from configuration-generation digest. Times are RFC3339; the last
  two fields retain the original restored completion; audit is the explicit
  Apply requester and expected trusted binding. Deadline is request time plus
  the pinned budget, not the old benchmark deadline.
- `result`: absent until a worker-attested terminal outcome; either `"applied"`
  or `"rolled_back"`. Undispatched cancellation/expiry has no result.

Apply starts queued with sequence 0 and an empty private lease. First `Lease`
atomically persists a new token and running state before returning the typed
Assignment plus `action: "apply"` (also
present in `job.action`). Ordinary benchmark Assignments omit action, preserving
old clients. The worker must dispatch by action, never rerun measurement for
Apply. Use only the pinned context/parallel recommendation, not arbitrary config.
Persist a job-owned rollback snapshot/watchdog before mutation.

### Durable reconciliation protocol v1

The worker lease body must include `"apply_protocol":1` for outstanding Apply
work (including recovery). Missing/other versions return 409 without dispatch;
ordinary benchmark leasing is unchanged. This is capability negotiation, not
attestation of worker implementation correctness. Legacy terminal Apply `/update`
reports are rejected with 400, including `result:"applied"` and rollback reports.
Nonterminal Apply updates still require action, exact next sequence, no summary
and existing progress rules; after preparation they are refused entirely.

`POST /worker/benchmark/reconcile` accepts exactly:

```json
{"worker_id":"example","job_id":"JOB_ID","lease":"LEASE_SECRET",
 "reconcile":{"transaction_id":"TRANSACTION_ID","generation":1,
              "phase":"prepared","durable":true}}
```

The same 16 KiB strict JSON decoder, per-request worker Bearer authentication,
machine scoping, lease validation and coordinator scheduler lock apply. Phase is
`prepared`, `applied`, or `rolled_back`; durable must be true. No commands, paths,
configurations, arbitrary errors or replacement provenance are accepted. A 200
contains the Job **after fsync/atomic replacement/directory fsync**. Each accepted
phase increments job sequence once; replay of the current phase returns the same
snapshot without a write. Prepared replay after a terminal decision returns 409;
status then identifies the final decision. Stale identity/generation, conflicting terminal
outcomes and invalid transitions return 409; malformed attestations return 400;
unknown/cross-machine jobs 404; invalid credentials 401; uncertain storage 503.
Authentication is rechecked even for terminal replay, including rotated secrets.

Existing authenticated `/worker/benchmark/status` with `{worker_id,job_id}` returns
Job including `apply_request` identity and `apply_phase`, never lease secrets.
`apply_protocol` is only permitted on `/lease`, `reconcile` only on `/reconcile`.
The store API itself requires callers to authenticate independently of the lease.

**Worker responsibilities and ordering:**

1. Before mutation, fsync a private rollback journal binding machine/job/lease,
   transaction ID, generation and pinned provenance; acquire durable local
   admission fence. Apply and health-check candidate under watchdog. Fsync a
   local prepared record before reporting `phase:"prepared",durable:true`.
   Coordinator accepts only running, unprepared, before deadline, not restoring,
   with two sequence slots remaining. It durably
   records `apply_phase:"prepared"`, running/validating, no result, **still fenced**.
2. Confirm matching prepared by successful response or authenticated status.
   Then fsync the local **irreversible applied decision and recoverable candidate
   configuration**, while retaining local admission fence. Watchdog and restart
   recovery MUST NOT roll back a durable applied decision. Only then report
   `phase:"applied",durable:true`. Coordinator accepts only from prepared and
   persists completed/done, result applied, restored false, failure empty.
   This releases the coordinator fence because both durable decisions agree.
3. Lost response/storage error is **not permission to roll back or unfence**.
   Keep committed configuration and local fence, replay the same phase or query
   status. After matching coordinator terminal agreement, durably record that
   agreement locally before releasing the local fence. A crash anywhere in this
   acknowledgment window repeats status/replay, never undoes a committed decision.
4. Before a durable local applied decision, worker can instead restore and
   health-check original configuration/service, fsync an **irreversible rolled_back
   decision**, then report `phase:"rolled_back",durable:true`. This is allowed from
   any dispatched nonterminal state, including prepared/recovery/cancelling.
   Coordinator persists failed/done, result rolled_back, restored true, failure
   execution. The same agreement/replay rules precede local unfencing. Failed or
   unverified restoration never sends this report; keep both fences. Before
   preparation existing recovery_required/restoration updates remain available.
5. Missing/corrupt local journal or missing/pruned coordinator history requires
   fenced operator reconciliation, never inference from remote status about
   physical state or automatic new execution. Opposite terminal decisions cannot
   overwrite one another. The trusted worker must serialize watchdog/parent and
   restart decisions under its local journal/admission lock.

Prepared is a durable authorization, not a renewable execution lease. It survives
coordinator restart, deadline and admin cancel **unchanged and fenced**. Repeated
lease returns the existing token with recovery_only true; this permits ONLY local
journal reconciliation, never fresh execution. Unlike ordinary recovery-only
work, a prepared transaction with durable local applied must preserve the applied
configuration, not restore the original. Unprepared leased work still becomes
recovery_required on restart/deadline and can only reconcile rolled_back.
Preparation reserves a sequence for the final decision. Prepared transactions
are never history-pruned; terminal records retain the existing bounded history.

Before dispatch, cancellation/expiry retain existing behavior: cancelled/failed,
sequence 0, restored false, no result/phase, no rollback assertion. Repeated admin
Apply still returns 409; status identifies the already consumed transaction.
Legacy Apply disk records without transaction identity fail closed at Open;
there is no unsafe automatic migration of possibly divergent physical state.
Ordinary legacy benchmark records remain readable. Older binaries reject new
fields, so downgrade requires operator reconciliation, not ledger sharing.

**Integration limit:** these are authenticated trusted-worker durable attestations,
not independent physical verification by Go. Worker/watchdog integration must
implement the irreversible decision and journal ordering above before opting in.
Production execution binding is documented in `docs/benchmark-targets.md`;
installation and real-host verification remain explicit operator actions. The
protocol does not enable production Apply. Python fixture integration implements
the irreversible decision and journal ordering. Disposable Go HTTP tests exercise
protocol/auth/replay and the production transaction with synthetic measurements;
real services and power-loss durability remain unverified.

## Durability, boundedness, and limitations

- At most one outstanding job per machine. At most **256 total jobs**, including
  at most **128 global terminal-history entries**. Oldest terminal history is
  pruned on mutation, also to make room for new jobs. Outstanding/recovery jobs
  are never evicted. With 256 outstanding jobs, `Start` returns `ErrCapacity`.
- Jobs are indexed by ID and outstanding machine. Ordinary `Get`/`Fenced` uses
  constant-time map lookup (plus bounded validation); `List` scans at most 256
  records. Mutations copy/encode the bounded snapshot, synchronously fsync it,
  and rebuild the active index. This favors safety over write throughput.
- `Open` requires an existing, trusted operator-owned data directory. It creates
  a dedicated `benchmark/` directory (0700), `jobs.json` and `writer.lock` (0600).
  Existing nonprivate files/directories, final-component symlinks and nonregular
  files are rejected. Ancestor paths and changes by the same OS owner are trusted;
  this is not a hostile-filesystem sandbox or cryptographically signed ledger.
- A nonblocking exclusive advisory lock rejects another cooperating writer with
  `ErrLocked`. Supported hosts are macOS and Linux; others return
  `ErrUnsupported` rather than writing unlocked. Use a local filesystem with
  reliable flock, atomic rename and file/directory fsync, not a shared/network FS.
- Writes use a 0600 same-directory temporary file, file fsync, atomic rename, and
  directory fsync before publishing state. Directory creation fsyncs its parent.
  **Any write/encoding failure poisons the store**, blocks all reads/mutations,
  and makes fence queries fail closed globally until Close/Open reconciliation.
  A write may have reached disk even if its method returned `ErrPoisoned`.
- Except for durable prepared Apply decisions (retained unchanged), restart
  converts every leased nonterminal job to recovery_required and durably
  saves that transition before returning. Existing recovery fences and tokens
  survive repeated restarts. Terminal restored acknowledgements survive restart.
- Load is capped at 1 MiB, 256 records, 8192 bytes per record, and JSON depth 8;
  it rejects unknown/duplicate/case-aliased fields, invalid provenance, duplicate
  IDs/active machines and invalid state invariants. A missing state file inside
  an **existing** benchmark directory is an error, not a fresh installation.
  A crash during first initialization may therefore need operator inspection.
- All exported errors are fixed sanitized sentinels. Raw OS/JSON errors, paths,
  tokens and worker text are never returned. History is bounded, not an audit log.
  There is no schema migration, signed provenance, distributed coordinator,
  automatic recovery acknowledgement, or actual power-loss verification here.

## Offline checks

From `go/`:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -cover ./internal/benchmark
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go vet ./internal/benchmark
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test ./internal/benchmark \
  -run '^$' -bench MapLookup -benchmem
```

Tests use temporary directories and fake evidence, never inference or real
coordinator data. They cover transitions, strict validation, replay, cross-machine
isolation, cancellation races, capacity/history, restart/deadline recovery,
permissions/locks, malformed/bounded loads, and uncertain writes both before and
after persistence. `FuzzDecode` supports additional offline fuzzing. Tests inject
write uncertainty and exercise actual rename failure; they do not simulate a
physical power loss or prove filesystem fsync behavior.

`ReportAllocs` benchmarks cover hit/miss/fence lookups at 1, 16, 64, 128 and 256
jobs, with setup, fsync and wall-clock sampling excluded. They are map-lookup
microbenchmarks, not end-to-end coordinator performance claims.
