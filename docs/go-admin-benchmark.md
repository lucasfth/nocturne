# Go admin benchmark orchestration and disposable worker integration

**Production execution is explicit-installation only, disabled by default.**
The Go control plane now has an operator-owned startup target binding and a real
Python worker runner backed by an independently installed root launchd service
adapter. No installation or live inference has been verified by this change.
See [installation and recovery](production-benchmark-install.md) and
[trusted startup targets](benchmark-targets.md). Production Apply remains disabled:
there is no installed real service-configuration transition binding.
Python remains the default gateway and does not implement these routes.

## Frontend contract

All routes are on the existing admin listener, require the current admin Bearer
credential and existing tailnet guard. Cross-origin browser requests are refused.
The admin credential is fleet-wide in the existing authorization model; every
operation explicitly scopes its machine and a job cannot be read under another
machine ID. Disabled/deleted machines return 404, never dispatch.

- `GET /admin/machines/{machine}/benchmarks`: retained job array, newest first.
- `POST /admin/machines/{machine}/benchmarks`: `{}` selects automatic numeric
  defaults (32768 context, 2 parallel slots, 600 seconds). Optional `advanced`
  contains only `context`, `parallel`, `budget_seconds`. Hide this in normal UI.
  Advanced bounds: context 8192..32768 in steps of 1024, parallel 1..2,
  budget 30..600 seconds. Zero selects default. Returns 202 and durable job.
- `GET /admin/machines/{machine}/benchmarks/{job}`: job/progress/results.
- `POST .../{job}/cancel`: body `{}`, returns job. Running cancellation does not
  release the scheduling fence until worker restoration is acknowledged.
- `POST .../{job}/apply`: body `{}`. Returns 202 only for a completed, restored
  benchmark with a separately enabled trusted Apply binding and matching
  model/build/code/config-generation provenance. Otherwise 409. No automatic
  Apply. Production has no enabled binding. The same job becomes queued with
  `action:"apply"`, retaining benchmark evidence and adding `apply_request`.
  Terminal `result:"applied"` means healthy candidate acknowledgment;
  `result:"rolled_back"` means verified prior-state restoration. Failed rollback
  remains `recovery_required` and fenced. Undispatched cancellation has no
  restoration assertion. A job can be applied only once.

By default production starts return 409 unsupported worker. `BenchmarkTargets`
are populated only from the protected, typed operator startup target file. The
operator must attest the reviewed installed execution capability; an HTTP request
cannot supply or change targets.
No user catalog/hash/path/command editing bypass is exposed. A disposable test
adapter supplies digests in the Go HTTP integration test only. Do not label a
409 as success or offer an automatic fallback to Q4. There is no download path.

Job JSON is documented in `go/internal/benchmark/README.md`: id, machine, typed
options, immutable schema/policy/fixture/hash/time provenance, state, progress,
sequence, sanitized failure, restored and optional summary. Lease tokens never
appear in admin snapshots. Fixed stages replace arbitrary logs. History retains
128 terminal jobs globally within 256 total records. No infinite UI log buffer.

HTTP failures: 400 invalid typed body/options; 401 credentials; 403 origin or
identity guard; 404 unknown scoped machine/job; 409 state conflict, replay or
unsupported adapter; 429 capacity; 503 unavailable/poisoned state. Error text is
sanitized. Never use it as an execution command or path.

## Worker bridge

Worker-only listener, existing per-machine Bearer authentication rechecked on
**every** request, including after credential rotation:

- `POST /worker/benchmark/lease` with `{"worker_id":"example"}` returns an
  assignment `{job,lease,recovery_only}`, or `{job:null}` when no work.
- `POST /worker/benchmark/status` with worker_id and job_id returns the scoped job.
- `POST /worker/benchmark/update` with worker_id, job_id, lease, and typed update
  object (sequence,state,progress,failure,restored,optional summary).

Exact sequence increments and job-bound random leases prevent replay and
cross-machine result submission. Strict bounded 16 KiB JSON rejects duplicates,
case aliases, unknown keys, arrays, extra documents and arbitrary command fields.
The bridge exposes fixed actions, no shell, SSH, URL, path or environment input.

Store Start and inference leasing share the coordinator lock. A queued job
immediately fences new inference; existing inference drains normally. Benchmark
lease refuses until zero active inference and fresh AC/battery>=50/nominal thermal/
no-active-human heartbeat. Restarted/expired execution stays recovery_required
and fenced; recovery-only lease never authorizes new benchmark execution.
Store failures close scheduling admission. The fence changes only the scheduler
view, not the prior heartbeat, machine configuration or service state.

## Implemented disposable execution boundary

`worker/benchmark_executor.py` supplies strict WorkerHTTP lease/status/update
polling, verified measured-row conversion and an opt-in FilesystemFixture binding.
`benchmark_watchdog.py` independently owns fixed-argv harmless children, deadline,
parent-EOF cancellation and journaled restoration. The fixture admission lock,
write-ahead prior-state journal and physical drain acknowledgment are exercised
by real subprocess tests. No browser-provided shell, executable, path or config
is accepted. Recovery with uncertain process ownership stays fenced rather than
killing a possibly reused PID. The fixture never claims inference success.

Apply uses a separately enabled fixture binding with content/config/revision
checks, health verification and durable rollback. The authenticated reconciliation
protocol pins a durable transaction ID and generation. Prepared retains both
fences; the irreversible local applied/rolled-back decision precedes coordinator
terminal reconciliation. Lost acknowledgments replay/status-check that decision,
never rollback a durable applied state. Local admission is released only after
matching coordinator terminal agreement and independently verified local health.
Legacy workers cannot complete Apply. This protocol is exercised with fixture
configuration in the original fixture milestone. Since the 2026-10-15 production
Apply implementation, startup defaults Apply off but can enable it for explicit
validated `production_apply` targets. See [the current RPC contract](production-apply-rpc-contract.md).

The real ephemeral HTTP test covers authenticated admin start, typed Python
polling, actual watchdog child, sanitized failure report, cancel, store/coordinator
restart, restoration, plus admin Apply through actual health-failure rollback and
persisted rolled_back. Apply's benchmark prerequisite is explicitly synthetic
fixture evidence, not an inference performance result. Python tests additionally
cover parent death, deadline, replay, stale bindings and commit uncertainty.

## Discovery and measurement contract

`benchmark_discovery.py` verifies bounded GGUF v3 metadata/tensor layout, actual
stored tensor types/counts, native context and SHA-256 from explicitly supplied
local paths. No traversal/download occurs. Default quality rejects every Q4 tensor
regardless of filename/declaration. Native context must support 32768; explicit
output reserve is inside that total budget. F32 vectors are permitted alongside
higher-precision matrices, but mixed matrix types remain MIXED. Quantization recipe
names such as Q5_K_M are not fabricated from tensor types. Unrepresentable legacy
catalogs refuse. Reinspection detects changed files; the old standalone runner
still reopens paths, so these snapshots are not immutable loaded-file proof.

Measured summary uses the actual long-row prompt_tokens, decode_tokens, decode_s
and decode_tps. output_allowance and reserved_tokens explain the 8-token guard and
up to 96-token packing slack. Counts are never padded to context; old exact-fill
records remain readable. 32768 is the per-slot context budget including output, not a claim of 32768
prompt tokens plus unlimited output. Two parallel slots allocate 65536 aggregate
context tokens (`context × parallel`), not 32768 shared between slots. Default context/parallel/budget are unchanged.
Actor is the authenticated `admin-bearer` role (not a named human); trusted code
revision and nonsecret config-generation digest are pinned at creation. Old jobs
with missing audit fields remain unknown and cannot satisfy explicit Apply.

## Installed execution and remaining activation boundary

- Real fixed-identity launchd service control, durable prior-state recovery,
  shared root-owned admission fence, authenticated narrow Unix RPC and independent
  unprivileged runner supervision are implemented. Worker/tunnel stay alive;
  only the allowlisted llama supervisor is disabled and booted out.
- Automatic flat bounded operator inventory uses strict GGUF verification. Model
  descriptors are inherited by the actual Backend via `/dev/fd`, not reopened
  from job paths. Root-owned readonly build/model ancestors and ACL checks enforce
  the installed trust boundary. Existing context/quantization quality gates remain.
- Agent polling is installed-capability gated. Real root health/idle-slot checks
  establish restoration; local durable receipts reconcile ambiguous reports.
- Cleanroom tests cover actual Go HTTP through the Python production transaction,
  launchctl abstraction, harmless process and durable result replay. Measurement
  evidence is synthetic in that test, not a hardware benchmark.
- Root installation, real launchd behavior, privileged socket credentials, reboot
  recovery and real inference must still be staged by the authorized operator.
  Source tests are not live verification. No other host or gateway was deployed.
- **Implemented, default off, not live-verified (2026-10-16):** production Apply
  configuration transition, health verification, rollback and durable reconciliation.
  `worker/benchmark_service.py` (`ProductionApplyAdapter`) implements the fixed configuration RPC;
  `go/internal/runtime/benchmark_targets.go` gates explicit production Apply targets.
  Root installation with `--enable-apply` and operator-provisioned presets remain
  required. See [activation and recovery](production-apply-rpc-contract.md).

Python gateway has no matching admin routes. No network model download, permanent
Apply, privileged installation, global cache clear or live-host trial was performed.
