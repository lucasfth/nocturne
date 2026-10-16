# Release security gates (2026-10-15)

Python remains the default and rollback runtime. This is local fixture evidence,
not deployment approval or live service verification.

## Ingress

Go admin credentials are validated before bounded body buffering, except explicit
admin assets and the separately signature-checked webhook. Worker ingress looks
up the bearer digest in the enabled registry before reading; it repeats the
machine-ID-bound check under the coordinator lock after parsing. Rotation,
disable and deletion rebuild the admission index atomically. Registry uncertainty
returns redacted 503, not a misleading 401, without buffering. Read deadlines are
cleared before long-poll/SSE handlers and after rejection. Native chunked tests
cover body bounds, stalled authenticated bodies and independent public service.

## Settings state

Both runtimes write only a sparse allowlisted overlay at
`data/settings/config.json`, with a 0700 parent and 0600 file. Root config stays
immutable, including auth/backend/extensions; existing base names remain
compatible even if they would fail the stricter API editing policy. Startup loads
the overlay before wiring. CAS, external edit detection, atomic replacement,
file/directory fsync and poisoned uncertain writes are tested. No systemd write
scope was broadened: only `data` is writable, never the executable/service root.

## Patched compiler and reproducibility

The official `https://go.dev/dl/?mode=json` response offered supported stable
Go **1.26.9** and **1.27.2**. Release builds select **1.26.9**, not the vulnerable
host Go 1.26.6. The official darwin/arm64 archive was downloaded into an isolated
user cache and its SHA-256 verified against that response before extraction:

```
go1.26.9.darwin-arm64.tar.gz
f9bb7c0a02506c5d9bf0d1eb1f7ee6c7684f844ae49558308a0427b830e022cc
```

No global compiler was installed or replaced. `go.mod` requires 1.26.9;
Makefile release targets additionally require that exact compiler and disable
VCS/time-dependent build IDs. To reproduce after verifying the official archive
for your platform:

```sh
export GO=/path/to/isolated/go1.26.9/bin/go
export PATH="$(dirname "$GO"):$PATH" GOTOOLCHAIN=local
make go-build go-linux-amd64
(cd go && go test ./... && go vet ./... && go test -race ./...)
uv run --locked pytest -q
GO="$GO" tools/shadow-compare.sh
```

Source/call-graph and actual stripped Linux binary scans used
`govulncheck@v1.9.0`, compiled with Go 1.26.9, database `https://vuln.go.dev`,
updated **2026-10-08 22:31:09 UTC**. Both reported **No vulnerabilities found**.
This includes symbol reachability analysis, not merely counting OSV entries.
It is bounded by the stated database snapshot and does not prove absence of
unknown vulnerabilities. Re-run scans against the current database at release.
Install the scanner into an isolated `GOBIN`, not the global toolchain.

Go full tests/vet/race, full Python tests, readonly-root overlay tests and
HTTP shadow comparison passed. Shadow: **126 cases, 124 equal, 2 intended,
0 unexpected** (only malformed authenticated admin-credit error differences).
Two same-source Linux builds were byte-identical. Linux was cross-built and
scanned on macOS, not executed on Linux. No live services, worker provisioning,
real payment backends or deployments were used.

## Historical combined production-adapter integration (2026-10-15)

Integrated the authenticated ingress security hardening with the production
adapter and frontend. Production bindings, durable reconciliation and installer
privilege boundaries are preserved.
No production worker RPC or adapter implementation changes were needed beyond
the security commit's preliminary authenticated ingress admission.

One integration regression was found in the synthetic real-Go browser fixture:
it still leased Apply without `apply_protocol: 1` and reported a legacy terminal
update. The current coordinator correctly refused that stale protocol. The
fixture now advertises protocol 1 and reports prepared/applied reconciliation
using the issued transaction and generation. Production admission was not relaxed.
These remain synthetic reports, not evidence of actual production Apply.

Historical verification of that integration tree, before production Apply (not current suite counts), on macOS with the isolated verified Go 1.26.9:

- Full Python suite: **535 passed**.
- Full Go tests, vet and race suite: **passed**.
- HTTP shadow: **126 cases, 124 equal, 2 intended, 0 unexpected**.
- Frontend typecheck/production build and **30 Chrome tests passed**, including
  real Go benchmark lifecycle/reconciliation and real Python settings UI.
- Both Go and Python readonly-root sparse-overlay/restart integration tests pass.
- Source/call-graph and stripped Linux binary govulncheck: **no vulnerabilities
  found**, with scanner v1.9.0 and database snapshot stated above.
- Two Linux builds were byte-identical, statically linked and stripped:
  **11,784,318 bytes**, SHA-256
  `deaa96078ad69bae771db7aaa570bf22fbc09256c0a05b70446831ddd35b5ab7`.
- Native Darwin ARM64 binary: **11,230,258 bytes**.
- Built frontend: **6 files, 446,131 bytes** (not committed).

At that historical milestone production Apply was disabled by the installed binding.
As of 2026-10-16, production Apply is implemented but defaults off, with explicit
validated target capability and root-service opt-in required. This is source and
fixture evidence, not live verification; see [the current RPC contract](production-apply-rpc-contract.md). Linux binaries were
cross-built and scanned, not executed on Linux. No deployment, live URL check,
service provisioning or real payment/inference operation was performed. Live
browser verification remains blocked by browser policy and is not implied by
local Chrome fixture results.
