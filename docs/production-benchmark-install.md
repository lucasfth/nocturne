# Explicit macOS benchmark service adapter

The standalone `worker/benchmark_service.py` installs an opt-in root launchd
service. Python remains the default gateway. Installation enables service
control only by default (`benchmark_execution:false`). Fresh installation with
`--enable-execution` explicitly attests the separately installed production runner.
There is no privileged execute RPC: the fence-aware agent launches the fixed
unprivileged runner; its watchdog owns benchmark children and the root daemon
independently owns the connection deadline and fail-closed recovery interlock.
A deadline is not a promise of restoration by that time. Production Apply remains disabled unless separately provisioned and explicitly
installed with `--enable-apply`; see [the fixed configuration RPC contract](production-apply-rpc-contract.md).

## Protected installation

Provision reviewed code independently under root-owned, non-group/world-writable
ancestors, without symlinks or ACLs. Do not promote a worker checkout into trust.
The daemon uses Apple's `/usr/bin/python3 -I -S`, a fixed environment, and stdlib
only; it never imports worker-controlled code. `-S` prevents global `site`, `.pth`
and `sitecustomize` startup hooks from running as root before validation. The root
entry point requires isolated/no-site flags and Python 3.9 or newer. The supported
interpreter is Apple's system Python: operators must validate that `/usr/bin/python3`
and its selected Apple runtime/stdlib are OS-protected and not redirected to a
third-party or writable runtime. These OS components are trusted, not covered by
the worker execution manifest; Python-level checks cannot secure an interpreter
that already executed untrusted startup code.

Installed files:

- `/Library/NocturneBenchmark/benchmark_service.py`: root-owned reviewed code.
- `installation.json`: root:0600 typed configuration: `version`, `worker_uid`,
  `lease_seconds`, `inventory`, `enable_execution` (boolean), and
  `execution_manifest` (SHA256 hex digest when enabled, otherwise null). Legacy
  configurations without the last two fields remain execution-disabled.
- `inventory`: exactly `model_directory`, `server`, `code_revision`. The first
  two are absolute protected root-owned paths (directory and executable
  `llama-server`, respectively); revision is a 40–64 character lowercase hex
  identifier. These are operator selections, never request parameters. The
  revision is inventory metadata, not runner attestation. Model contents must
  also be independently provisioned as trusted readonly data before execution.
- `state/`: root:0700; root:0600 durable journal and exclusive daemon lock.
- `admission.json`: root:0644 shared admission fence in the protected installation
  directory. Workers can read it, never write or remove it.
- `/var/run/nocturne-benchmark/control.sock`: root-owned group-connectable socket
  in a protected directory; filesystem validation uses `/private/var`.
- `/Library/LaunchDaemons/io.nocturne.benchmark-service.plist`: root watchdog.

Every installation verification checks the allowlisted worker, llama, and tunnel
identities. All must run as `nocturne-worker`; worker/llama commands are fixed.
Unsupported identities, unsafe paths/ACLs, abandoned process groups, and root
redirection refuse. Existing installer SSH restrictions remain mandatory.
**Worker and tunnel are never disabled, booted out, enabled or bootstrapped.**
Their prior state is observed and compared, not repaired by this daemon.

## Operator installation (deployment commands, not tests)

Use independently reviewed protected paths and the actual installed revision:

```sh
sudo env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
  /usr/bin/python3 -I -S /trusted/package/benchmark_service.py install \
  --lease-seconds 600 --model-directory /trusted/models \
  --server /trusted/bin/llama-server --code-revision "$INSTALLED_HEX_REVISION"

sudo env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
  /usr/bin/python3 -I -S /Library/NocturneBenchmark/benchmark_service.py verify

# Separate explicit operator approval required:
sudo /bin/launchctl bootstrap system \
  /Library/LaunchDaemons/io.nocturne.benchmark-service.plist
```

For execution, append `--enable-execution` to the **install** command only, after
independently provisioning the reviewed full `worker` package at
`/opt/nocturne/worker`, the protected venv at `/opt/nocturne/.venv`, and its runtime
at `/opt/nocturne/python`. The agent must include the production polling and
admission checks before lease and execution. Source-marker checks are a guard
against old agents, not a substitute for operator code review.

The installer verifies all runtime symlink hops (ownership, ACLs, writable
ancestors, bounded traversal within the installer runtime boundaries), required
runner modules, dependencies and startup hooks without executing/importing them.
It requires a single `.venv/lib/python3.*/site-packages` and no competing `worker`
package there. Only explicit opt-in creates the root-owned, literal-path
`nocturne-production.pth` containing `/opt/nocturne` plus newline. This enables
`/opt/nocturne/.venv/bin/python -I -m worker.benchmark_production` from `/`;
PYTHONPATH and the working directory are not module-discovery mechanisms. Existing
conflicting `.pth` content is refused, never repaired. All other protected runtime
startup hooks must be independently reviewed too. The unprivileged runner
intentionally stays `-I`, without `-S`: its protected `.pth` is required for module
discovery. Do not copy the root daemon flags to that runner.

The config manifest is a deterministic SHA256 over installed package/runtime paths,
link targets and file hashes, including the `.pth`. Startup verifies it before opening the socket; missing, changed or unsafe code
fails closed. The verified manifest, protected Apply policy and interpreter are
pinned for this daemon lifetime, so connections and health polling do not rehash
large model/runtime inventories. Ping uses the connection's verified capability, without walking or hashing
the runtime or models. Connections and transaction verification must match the
same startup config/generation; updates require a daemon restart. This cache relies
on the protected installation being immutable to unprivileged users. It includes
dependencies and bytecode: provision caches offline and do not mutate the installed
runtime while serving. Validate full verification latency in staging.
Model inventory must be nonempty, flat and capped (64 GGUFs, 512 GiB total,
1024 directory entries); each model and server must be root-owned, ACL-free and
have **no write bits** (e.g. 0444 models, 0555 server). Model content hashes remain
streaming-verified by the unprivileged runner, not repeatedly hashed by root.
Artifact descriptor-layer ACL verification is a separate hardening concern; this
installer/daemon checks ACLs at its boundary and trusts concurrent root activity.

Install is fresh-only and never invokes launchctl. Initial admission is fenced;
startup journal recovery or explicit startup baseline health validation opens it.
Without a journal, startup leaves the fence active until service states and
installation generation are stable and loaded llama passes health/idle slots
(or unloaded llama has a closed port and no inference processes). Failed health
requires correction and a daemon retry, not manual fence deletion. `verify` is readonly, with no launchctl
or HTTP calls. Config/code updates are offline operator actions, not JSON CRUD.
Errors expose only a static refusal, not paths, credentials or subprocess output.
Never delete an outstanding journal or fence to bypass recovery. Booting out the
root daemon during a transaction prevents KeepAlive recovery; re-bootstrap it
or reconcile offline under operator control.

## Worker/poller interface

`ServiceClient.admission_fenced()` is a connection-independent local check.
Missing, unreadable, malformed, linked, ACL-bearing or worker-writable state
returns **True**. It does not require the transaction socket, so the worker can
continue polling while another client holds the service lease. The separately
integrated inference agent must check it **before leasing work and again before
execution**, and decline inference while true. Only a verified installed adapter
may be selected for production; a fence check alone is not execution attestation.
Coordinator admission must also drain already accepted work before destructive
service shutdown. A check is not a cross-process atomic lock: an execution
already crossing admission is terminated by llama bootout, and drain is not
acknowledged until the server is absent. The worker and tunnel remain alive.

A context-managed `ServiceClient` provides `snapshot`, `fence`, `drain`,
`restore(prior)`, `verify(prior)`, `release`, `ping`, readonly `inventory()`, and
readonly `admission()`.
Return types are `PriorState(generation, config, service)`,
`DrainAck(generation, active, service_stopped)`, and typed `Inventory`.
The inventory is supplied by the root daemon from its protected config, not
accepted from the worker. The client exposes no writable privileged journal.

The Unix peer is kernel-authenticated with macOS `getpeereid`: only the installed
worker UID may call; the client requires a root peer. Any process sharing that
worker identity can request a temporary service outage, so it must not be shared
with untrusted tenants.

Protocol: newline JSON, at most 4096 bytes, exactly `{"op":"snapshot"}` or another
listed operation. No path, PID, argv, environment, import, job payload or supplied
prior state is accepted. Duplicate/unknown fields and nonfinite numbers refuse.
Successful replies are `{"ok":true,"value":...}`; failure closes the connection.
`ping` reports `service_control:true`; `benchmark_execution` is true only for an
explicitly enabled, currently verified installation manifest. Do not infer
restoration from EOF. Use one thread per connection; ping every three seconds
while otherwise idle. The five-second idle timeout and absolute 10–600 second
lease cannot be extended by traffic or partial bytes.

## Admission attestation security review

Execution-enabled connections may send exactly `{"op":"admission"}` after
snapshot and fencing. The daemon checks llama unloaded/disabled and the published
active fence before and after observation. This ongoing attestation deliberately
does not require a closed port or absent inference: the benchmark's owned server
may be running. Strict initial drain and final restoration absence checks remain
mandatory. There is no client PID, path, command or environment.
It reads only fixed `launchctl print system/io.nocturne.worker`, requires one
positive PID from a loaded job, then uses bounded `/bin/ps -p PID -o comm=` and
`lstart=` (two-second deadline, 4096-byte output limit each). A second fixed
launchctl print must report the same PID. The executable must exactly match the
protected, resolved managed `.venv/bin/python`, with executable file permissions;
missing, ambiguous, malformed, changed or unexpected observations refuse.
The reply is exactly `{worker_pid:int, worker_executable:str, worker_start:str}`.
No runtime commands supplied by a caller are executed, no code is imported, and
no process is signaled by admission. Existing socket authentication, absolute
lease, strict message schema, static errors and recovery behavior remain intact.

This is a point-in-time observation, not a process lock or worker readiness API.
PID reuse/exit after observation remains possible. The unprivileged runner must
compare all three fields with its own process snapshot and narrowly exempt only
that matching worker and its own verified watchdog. All other competitors still
block; a basename-wide Python/worker exemption is unsafe. The standalone stdlib
root `benchmark_service.py` is service control, not a benchmark competitor.
Runner competitor matching is a separate integration responsibility, not enforced
by this RPC. Coordinator drain and the worker's shared-fence checks remain required.
The trust boundary excludes concurrent malicious root changes and renamed/manual
inference executables; no broader absence or live production readiness is claimed.

## Fence and independent recovery watchdog

The root daemon owns the lease deadline independently of the worker connection.
Before mutation it durably journals the generation and all three effective
loaded/disabled states, then publishes the shared active fence (atomic replace,
file and directory fsync). Only llama is disabled and booted out. This removes
its supervisor as well as its runtime, preventing launchd/supervisor restart.
Drain requires llama unloaded/disabled, localhost port 8080 refusing connections,
and a bounded system-wide executable snapshot without `llama-server` or
`llama-supervisor.sh`. Tunnel port 18701 is deliberately not drained.

Before fencing, root binds the session to Darwin kernel `LOCAL_PEERPID` and
`getpeereid` credentials, checks the configured unprivileged worker UID against
the startup-attested execution installation, and attests the protected
interpreter `comm` and fixed isolated production-runner argv. Fencing requires
this execution-enabled source attestation, not just worker-UID access. Kernel `proc_pidinfo` start seconds **and microseconds** are checked around
attestation and persisted with PID/UID and active session evidence. No peer PID
comes from worker JSON. The long-lived agent may use snapshot/ping preflight;
it cannot fence as the runner. The actual runner connects from its own process.

EOF, invalid RPC, idle timeout, and lease expiry close the socket (allowing the
runner's one-second heartbeat to cancel) and mark fenced records
`restore_pending`. Child absence alone is NEVER teardown evidence: a delayed
spawn may still complete after cancellation. While the peer is alive, root keeps
llama disabled/unloaded and admission fenced, including in the session's finally
block. launchd KeepAlive retries recovery after exit; startup checks the persisted
peer before exposing the socket. Only independently established process absence
permits automatic restoration. PID reuse/start mismatch, missing legacy identity,
and query errors fail closed, without signaling any persisted PID.

An explicit restore RPC from the same re-attested runner durably closes the
session under the trusted installed module contract: owned children have been
cleaned up and no future spawn is permitted. That connection cannot fence again.
This allows normal completion while the runner is still alive. If the deadline
already closed the socket, the runner must exit; root waits for independent exit
evidence instead. Before the first restore bootstrap the daemon additionally
requires inference absent and port closed, even if an unexpected llama service
appeared.
Lingering benchmark children therefore block restoration; no arbitrary or
persisted PID is killed. Renamed/manual executables are outside this fixed-name
contract. Root does not own benchmark child process groups; the fixed
unprivileged production watchdog does. SIGKILL of that watchdog may require
operator recovery and must not be mistaken for successful restoration.

Restoration changes only llama to its original effective loaded/disabled state.
Originally loaded-but-disabled llama is temporarily enabled to bootstrap, then
disabled again. An absent disabled override may become explicit false; effective
state, not the launchd database byte representation, is preserved.

**The shared fence stays active throughout restore and health verification.**
Verification requires unchanged code/config/service generation, exact prior
states (including untouched worker/tunnel), and direct bounded localhost
`GET /health` with `status:ok` plus `/slots` with every slot
`is_processing:false`. No proxies or redirects are used. If llama was originally
unloaded, both closed port and absent inference are required instead. There is
no fabricated worker/tunnel HTTP readiness check. Only verified `release` clears
the journal and publishes inactive admission. Failed health, remaining children,
changed installation, or changed worker/tunnel state retains the fence and
journal; root recovery retries rather than releasing admission on disconnect.

### Completion before service mutation

If validation fails after `snapshot` but before `fence`, the existing
`restore(prior)`, `verify(prior)`, `release` sequence completes without service
mutation. Only that connection's server-held snapshot is used; no historical prior
can be supplied over RPC. Root requires an exact unchanged generation and all
service states, checks health/idle slots (or closed port and absent inference for
an unloaded prior), then rechecks the exact snapshot. It durably journals the
verified prior and unchanged-completion receipt before acknowledging restore.
Verify and release recheck the evidence; recovery of this receipt never repairs
changed services by applying an old prior. Changed state or failed health refuses
completion. Normal release clears the journal only after verification. This path
does not relax restoration after fencing or mutation.

## Tests

```sh
uv run pytest -q tests/test_benchmark_service.py
```

Tests use temporary directories, fake launchctl, socket pairs, mocked HTTP and
harmless children. No install, deployment, live launchctl or inference is run.
Real macOS launchd descendant teardown, reboot behavior and peer credentials
still require an operator-approved staged test.


## Production Apply enablement and authorized benchmark prerequisites

No live installation, model load or deployment is performed by this change.
For an authorized benchmark, the operator must first verify the target
machine identity, explicit owner consent, protected reviewed package/runtime and
system interpreter, immutable model/server inventory, working restricted tunnel,
healthy original llama service, shared admission fence, durable writable root
state storage, and fresh AC/battery/thermal/idle telemetry. Stage launchd teardown,
peer identity and readiness on that exact installation before authorizing a real
benchmark. Neither tests nor generated evidence establish inference quality.

Apply is a second authorization. Independently provision the reviewed preset
catalog and baseline described in the RPC contract, then fresh-install with both
`--enable-execution --enable-apply`. Existing installations require an offline
reviewed upgrade/reconciliation procedure; do not overwrite a live journal or
rerun fresh-only install. Record actual installation generation, execution and
Apply manifests, model/build digests and revision in the protected Go target
file. Only then add its explicit `production_apply` capability. The gateway
manifest field is operator attestation, not remote proof. Reloading these
startup bindings requires the normal authorized service restart.

Run a new successful benchmark on that installation, retain its durable local
completed/restored receipt, inspect quality and measurements, then use the
existing explicit confirmed admin Apply action. It selects only an exact
catalog model/build/context/parallel/cache match. Root changes actual supervisor
model/context/runtime flags while preserving schedule, power policy, alias,
worker and tunnel. Root health validation is not a new inference quality test.

Local applied decisions are irreversible once durable; coordinator response loss
must reconcile, never trigger a second Apply or rollback an applied decision.
Before that decision, recovery restores exact prior supervisor bytes and effective
llama state. Both sides remain fenced until verified terminal agreement. Never
delete root/local journals or immutable receipts to escape a blocked transaction.
Stale/missing benchmark provenance or a changed baseline requires operator review
and a new benchmark, not retroactive evidence. See RPC contract for bounded
readiness, pre-prepare abort, persistence failures and reboot recovery limitations.
