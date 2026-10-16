# Go startup benchmark target opt-in

The Go runtime can bind trusted benchmark targets at startup from **only**
`<runtime-root>/benchmark-targets.json`. No HTTP endpoint, admin setting, environment
variable, or job can supply this file or an alternate path. Absence disables
bindings. Invalid/present-but-untrusted files fail startup with a static error.
Changes require a coordinator restart; this is not a live configuration API.
Python gateway parity is not implied.

## Installation and trust boundary

First review and explicitly install the worker production service and execution
capability using [the production installation procedure](production-benchmark-install.md).
Source availability, a service-control-only installation, a passing cleanroom test,
and a responding worker are **not** sufficient installation evidence.

A privileged operator must independently review the installed worker code,
interpreter/dependencies, root service, immutable artifacts, watchdog, durable
restoration and admission fencing, and confirm execution capability is enabled.
Record the installed execution manifest digest, code revision, configuration
generation and model/build digests from that reviewed installation. The manifest
field identifies what the operator reviewed; the gateway does **not** fetch,
recompute, authenticate, or cryptographically validate remote installation from
this file. There is no remote attestation protocol here. All capability booleans
are operator assertions, not gateway discoveries. Missing/false capabilities
refuse startup. Do not assert capabilities that have not been installed and
reviewed. Existing production sensor/competitor restrictions remain unchanged;
a binding does not guarantee a successful measurement.

The file must be a root-owned regular file, mode **0600**, with one hard link.
Its runtime root and every ancestor through `/` must be root owned and not group
or other writable. Symlinks are forbidden at every component, including the file;
use the physical path (for example `/private/...` rather than a symlink alias on
macOS). Sticky writable ancestors such as `/tmp` do not qualify. Unix descriptor
relative `openat`/`O_NOFOLLOW` traversal pins and checks each parent, and `fstat`
checks the opened file. An absence probe permits ordinary unprivileged deployments
without opting in; it never authorizes a present file. No chmod/chown, service
installation, provisioning or deployment is performed by gateway startup.

Install the file through a reviewed privileged process using atomic replacement
and file + parent-directory fsync. Keep the privileged writer exclusive; do not
modify a live file in place. The gateway needs permission to read the 0600 file;
this opt-in is deliberately unavailable to an unprivileged coordinator without
that access. Do not broaden file permissions or run a gateway as root merely to
make a test pass.

## Schema v1

Example shape only; repeated hex values below are placeholders, **not usable
installation evidence**:

```json
{
  "version": 1,
  "targets": [{
    "machine": "worker-1",
    "model_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "build_digest": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "code_revision": "dddddddddddddddddddddddddddddddddddddddd",
    "config_generation": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "installation": {
      "capability_version": 1,
      "manifest_sha256": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
      "reviewed_installation": true,
      "service_control": true,
      "benchmark_execution": true,
      "independent_watchdog": true,
      "durable_restoration": true,
      "admission_fencing": true,
      "artifact_verification": true
    }
  }]
}
```

Maximum 64 KiB, 1–128 unique targets, schema/capability version exactly 1.
Every field shown is required. Machine IDs must match enabled entries in the
configured machine registry. Digests and configuration generation are lowercase
64-character SHA-256 identities; revision is lowercase 40- or 64-character Git
hex. Unknown keys, duplicate keys, case aliases, trailing JSON and invalid types
are rejected. No commands, URLs, local/remote artifact paths, credentials or
arbitrary configuration are accepted.

**Apply defaults to false.** Only an additional per-target `production_apply`
object enables coordinator dispatch, and only after separately installing and
reviewing the real root Apply adapter and production runner:

```json
"production_apply": {
  "protocol": 1,
  "manifest_sha256": "<installed apply_manifest SHA256>",
  "durable_decisions": true,
  "configuration_rollback": true,
  "reviewed_installation": true
}
```

This is a protected operator assertion, not remote cryptographic attestation.
Never copy placeholder values or enable it based on source/test availability.
The worker independently refreshes installed `production_apply` capability before
advertising the reconciliation protocol. Root checks its pinned protected preset
manifest, immutable model/server artifacts and current configuration; the runner
requires the same completed local benchmark, passing quality evidence, provenance
and original supervisor digest. See [the RPC and recovery contract](production-apply-rpc-contract.md).
A stale or legacy benchmark without this receipt cannot be applied. Automatic
benchmark completion never applies a configuration. Existing admin Apply remains
an explicit separately confirmed action. No global startup default enables it.

## Cleanroom verification

`go/internal/app/benchmark_production_test.go` uses real authenticated Go
`httptest` admin and worker HTTP endpoints and Python `BenchmarkHTTP` lease/update,
then calls the actual production `transaction`. It uses disposable journals,
the real root-side `ServiceAdapter` with fake launchctl, and a harmless reaped
Python subprocess. Artifact opening/digests and measurement execution are mocked;
success evidence is explicitly synthetic. It verifies both synthetic completion
and execution-failure restoration, durable replay receipt, released fences, and
refusal of Apply without explicit capability. It neither starts launchd nor accesses installed worker paths,
real models, payment backends or live hosts. It does not validate macOS root
installation, real inference, artifact integrity, privileged IPC, or independent
watchdog crash recovery.

```sh
(cd go && NOCTURNE_TEST_PYTHON="$PWD/../.venv/bin/python" \
  go test -race ./internal/runtime ./internal/app)
```

The HTTP test skips if Python/httpx is unavailable; set `NOCTURNE_TEST_PYTHON` to
an interpreter with project dependencies. Schema tests are unprivileged; protected
filesystem tests deliberately verify rejection rather than provisioning a
root-owned installation.


`TestBenchmarkProductionApplyHTTP` adds actual disposable supervisor-file mutation,
root ServiceClient/session IPC and authenticated Go prepared/applied/rolled_back
reconciliation. Seven scenarios cover success, health rollback, lost root and
coordinator acknowledgements, and crashes before/after rename. Fake launchctl and
OS identity/protection checks are test substitutions; benchmark quality evidence
is synthetic, never a measured inference claim. The production Python gateway
still does not gain the Go benchmark control plane.
