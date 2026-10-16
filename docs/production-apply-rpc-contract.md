# Production Apply RPC contract

Opt-in root adapter only. No installation, activation or live services are used
by tests. The installed production runner integrates this protocol with the Go coordinator;
Python remains the default gateway and does not gain Go control-plane parity. This is a fenced local participant, not a coordinator.

## Wire contract

Requests are exact-field JSON objects, newline delimited, maximum 4096 bytes;
responses are bounded to 16384 bytes. Duplicate keys and extra fields refuse.
The authenticated exclusive connection has a fixed lease (heartbeat does not
extend it). `ping` advertises `production_apply:true` only when enabled.

* `presets` (only `op`) is readonly. Returns `generation` (pinned catalog SHA256),
  `config_generation` (current supervisor SHA256), and `presets`, keyed by preset
  SHA256, containing model/server digests and context/parallel/cache bindings.
  Runner must match benchmark model, build and context, not merely a preset name.
* After `snapshot`, `prepare` has exactly `op,preset,transaction_id,generation,
  config_generation`. Transaction ID is lowercase 64hex, generation is integer
  **1** (not boolean); config generation is lowercase 64hex matching actual prior
  supervisor bytes. Preset is a catalog entry digest. Stale service snapshots,
  stale config digests and previously finalized transaction IDs refuse before
  service/config mutation. Root journals original bytes, service state, peer,
  transaction and both config digests before fencing/draining.
* `abort` has exactly `op,preset,transaction_id,generation,config_generation`
  (same types/bindings as prepare), with no preceding snapshot required. It is
  authoritative reconciliation ONLY for a never-prepared transaction. Under the
  exclusive root daemon lock, root requires no active journal and no immutable
  receipt for this transaction (including applied receipts), verifies a stable
  current service snapshot, health and current supervisor digest against the
  ORIGINAL benchmark-bound `config_generation`, then durably journals a no-op
  **rolled_back** decision and immutable receipt and retains an admission fence
  until finalize. It never changes service/config. Target digest still binds the
  requested preset; original digest binds the verified unchanged configuration.
  It returns the same decision shape as status. On lost response, query status:
  a matching active decision follows recover/finalize, not another abort. Any
  active journal or prior receipt refuses abort; stale original digest refuses
  `apply_stale_config` and requires operator reconciliation, never substitution
  of a newly observed digest. Missing prepare is NOT itself rollback evidence.
* `apply` (only `op`) installs the selected catalog-derived configuration,
  restores prior effective service state, independently verifies config and
  health/idle slots, then persists **applied**. This is an irreversible local
  decision. Admission remains fenced. There is no `commit` RPC.
* `rollback` (only `op`) is allowed only before applied. It restores and verifies
  actual original bytes and service state, then persists **rolled_back**, keeping
  journal and fence. A repeated rolled_back rollback only verifies the decision.
* `status` (only `op`) is readonly and returns `transaction_id,generation,preset,
  phase,original_config_digest,target_config_digest`, or null before any apply.
  After finalize it returns the latest immutable receipt's decision, not idle.
* `recover` (only `op`) independently recovers the journal, returning status.
* `finalize` has exactly `op,transaction_id,generation,agreement`. Agreement is
  `applied` or `rolled_back`, matching the terminal local decision and expected
  transaction/generation. The reattested trusted runner must report this only
  after authenticated coordinator agreement. Root independently verifies actual
  local config, service states and readiness; client status is never service
  evidence. Finalize writes a per-transaction immutable receipt and latest-status
  copy durably, marks the journal finalizing, and durably writes idle **before**
  opening admission. In-memory pending state is retained until idle is durable.
  It ends the connection. Repeating finalize for the latest receipt resolves a
  lost acknowledgement without rewriting it, only after fresh stable config/service
  health verification and verified admission opening. Any persistence uncertainty
  poisons the daemon and requires process restart, including receipt-only retries. Older receipts remain on disk;
  status exposes the active transaction or latest receipt, not an arbitrary query.

Apply/explicit rollback promise runner child teardown and no future spawn.
They require the original reattested peer. Recover/finalize permit a newly
reattested runner only after independent kernel evidence that the original peer
is absent. Live peers, PID reuse and ambiguous process queries refuse takeover.
The trusted runner, not a caller-supplied status blob, supplies coordinator
agreement. The root daemon does not contact the coordinator itself.
Legacy restore/release cannot bypass the apply decision protocol.

EOF/restart before a durable decision rolls back only after independent runner
absence, verifies original config and retains rolled_back journal + fence.
Applied recovery verifies target config and service health without rollback;
rolled_back recovery verifies original config. Both retain admission fencing
until explicit finalize. Persistence errors deny acknowledgement and trigger
best-effort refencing plus daemon exit; an admission-open write can have become
visible before its fsync reports failure, but only after durable idle and receipts.
A new daemon rereads
what actually became durable. Crashes during finalize can leave a receipt plus
terminal journal; retry is safe, and receipt bytes must match. A crash after idle persistence but before admission opening is reconciled on
restart or receipt-only finalize. Readiness polling has a 30-second budget
(plus the final bounded probe), at 250ms intervals; no wire field can extend it.
Rollback durably enters rollback_restoring before bootstrap; restart resumes
that stage without tearing down an already-loading original service. Admission
stays fenced during delayed readiness; daemon restart can continue progress.
Readonly status/listing connections do not invoke recovery on disconnect.

## Protected operator provisioning

Fresh installation accepts `--enable-apply` only with `--enable-execution`.
Provision `/opt/nocturne/production-apply-presets.json` as root-owned, ACL-free,
without symlinks or writable ancestors. Exact fields: `version:1`,
`supervisor_sha256` (reviewed baseline digest), `presets` (1–16 entries).
Each entry has exactly `model_sha256,server_sha256,ctx_size,parallel,cache_type_k,
cache_type_v`. Identifier: SHA256 of UTF-8 JSON, sorted keys, compact separators.
Context is 512–262144, parallel 1–16, divisible context; cache types are `f16`,
`q8_0`, `q4_0`. Catalog digest is pinned as `apply_manifest` in installation.json.
Baseline is `/Library/NocturneBenchmark/supervisor-original`, root:0600.

Model digests select among protected immutable `/opt/nocturne/models/*.gguf`
files, never RPC paths. Inventory is bounded to 1024 directory entries and 64
models. Filenames must match `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\.gguf`;
duplicate model digests refuse ambiguous selection. Server digest must match
immutable `/opt/nocturne/bin/llama-server`. Catalog entries form the digest
allowlist. Full artifact verification happens once at daemon startup, before
recovery, inference mutation, or socket exposure. It includes all existing
ownership, ancestor/symlink, ACL, immutable model/server checks and full
runtime/dependency and eligible model/server content hashing. A dedicated
3600-second startup alarm fails closed; it consumes no runner lease. Installations
that cannot attest within this budget remain unavailable, not partially trusted.

The daemon pins that verified configuration, generation, policy and resolved
runner interpreter for its lifetime. Session fingerprints, snapshot/restore and
readiness use this cached attestation, not inventory-sized hashing. This relies
on the existing protected-root trust boundary: unprivileged peers cannot modify
any attested artifact or ancestor. Concurrent root modification is outside the
boundary. **Stop the daemon and runners before any offline installation update;
restart and fully reattest before use.** This includes configuration, catalog,
code, runtime, models, server, plists, permissions and ACL changes. Root must not
update these in place while the daemon runs. The sole live artifact mutation is
the daemon's journaled supervisor preset replacement, whose protected path and
exact allowed bytes are still checked on use. No ownership or ACL checks have
been removed from startup attestation.

Production sessions inject the pinned peer attestor, retaining kernel peer UID,
PID/start-identity and exact interpreter/argv checks for every mutating RPC.
Standalone `runner_peer(conn)` retains full independent execution attestation.
Apply RPCs share an absolute budget of at most 100 seconds from connection start
(and never beyond the lease), below the runner handshake budgets. Commands use
the remaining budget; mutations and successful replies check expiration. A
successful readiness probe is rejected if it finishes beyond its 30-second
readiness window or RPC deadline. Direct HTTP probes have a four-second aggregate
budget with socket shutdown to bound trickling headers/bodies. Filesystem syscalls
are not hard real-time guarantees: a stalled fsync may delay return, but cannot
turn an expired operation into a successful reply or authorize a later mutation.
Connection cleanup has a separate 100-second recovery budget, cannot acknowledge
the expired RPC, and retains journal/fence state on failure for subsequent
recovery. No full attestation is performed during recovery.

The reviewed supervisor must have exactly one literal `M=$R/models/NAME.gguf;`
assignment and one `--ctx-size 65536 --parallel 2 -ctk q8_0 -ctv q8_0` sequence.
Only these literal bytes are substituted, using validated inventory filenames
and bounded catalog flags. No shell evaluation or request-supplied text is used.
Alias, binary, schedule/power policy, disabled marker, host/port and other bytes
remain unchanged. Unknown supervisor versions refuse. Supervisor stays root:0755;
private journal retains exact original bytes. Installation generation binds the
baseline rather than mutable variants. Offline edits require operator
reconciliation. Worker and tunnel are observed, never restarted or repaired.

Tests use disposable real files and fake launchctl. Run:
`uv run pytest -q tests/test_production_apply_service.py tests/test_benchmark_service.py`.
macOS peer credentials, ACL enforcement, reboot and live readiness require
approved staging; unit tests are not production verification. Disposable Go HTTP integration is
covered separately; no Python gateway control-plane parity is implied.
