# Offline Python / Go HTTP shadow verification

Run from the repository root on a system with Go and Python prerequisites:

```sh
./tools/shadow-compare.sh
```

Requires `uv` and `go` (overridable with `UV` and `GO`). The Python reference is the actual current `gateway.app.Coordinator`,
not a reimplementation. The Go helper uses `internal/runtime.New` and the real
edge, public, worker and admin handlers. Both bind ephemeral loopback listeners.
Every config, ledger, machine registry, asset stub and helper binary is created
inside a temporary directory and deleted afterwards. No production service,
wallet, operator host or runtime configuration is involved. Explicit `fake`
Lightning is mandatory in the generated fixture. No real payment is possible.

Disposable configuration files are created with mode `0600`; worker journal
fixtures use mode `0700` directories. The smoke helpers set these explicitly so
verification does not depend on the operator's umask. Production permission
checks remain unchanged.

FX and energy use injectable offline providers at a fixed clock:
2026-10-15 17:00 UTC, 100000 DKK/BTC and 100 DKK/MWh + 1.2 DKK/kWh tariff.
Python rejects non-loopback socket connects. Go replaces the default HTTP
transport with a rejecting transport, and injects both network providers; the
fake backend has no network path. This is not an OS network sandbox. The helper
uses runtime's wired handlers but does not exercise the production `Serve`
lifecycle or fixed ports. Dedicated runtime tests cover that lifecycle.

`web/dist` is absent at the baseline. Both sides receive identical temporary
`index.html` / `assets/shadow.js` stubs. Python's module `__file__` is redirected
to the copied temporary gateway directory solely for its existing asset lookup.
The real SPA build is not verified and no checkout asset is created.

## Comparison rules

Status codes and parsed JSON objects are compared. Array ordering, null versus
empty arrays, errors, amounts, usage and text are preserved. JSON numeric values
compare numerically (1000 and 1000.0 are equivalent), not by serialization bytes.
SSE is parsed as an ordered sequence of `data:` JSON events plus `[DONE]`;
whitespace framing and arrival timing are not tested. Static text bodies compare
exactly. This runner does not compare response headers or raw HTTP framing.

Only explicit nondeterminism is normalized:

* Generated signup key ID and plaintext-once token are bound per response pair.
* Invoice IDs (including their occurrence in fake BOLT11/payment_hash), chat IDs
  and lease IDs are bound per response pair. Reused IDs retain their bindings.
* Non-null numeric `created`, `expires_at`, `paid_at`, `added`, ledger `ts`,
  `last_used`, `heartbeat_age` and `heartbeat_age_s` become timestamp markers.
  They must be nonnegative numbers, not booleans. Null remains null. Billing and
  worker monotonic time intentionally remain real so leasing and cancellation
  work. Energy/FX clocks are fixed instead of normalizing their public values.

These timestamp replacements mean exact expiry duration/clock arithmetic is
outside this shadow check; billing/invoice tests cover those separately. IDs are
not cryptographically or syntax-validated by this comparison. An optional
Python `segno` install would add `qr_svg`; Go's documented missing optional QR
support would appear as an unexpected difference here, not be removed silently.

## Historical run after the empty-ledger fix

```
SHADOW cases=104 equal=92 differences=12 intended=12 unexpected=0
```

All public application routes, five worker routes and admin application routes
are represented, including authenticated/unauthenticated errors, static public
files, admin stubs, signup, real worker registration/heartbeat/leasing,
nonstream and SSE chat with split results/think stripping/usage, in-flight worker
failure/refund, stale result/fail, registry add/update/disable/delete, credit,
fake invoice pending/payment/idempotency, balance, summary and ledger.
This is representative coverage, not exhaustive combinations of every branch.
It does not prove real Lightning interoperability, deployed operation, slow
reader behavior, raw HTTP security, all scheduler races or actual web UI assets.

### Exact observed differences

Twelve deliberately retained differences were already documented in
`go/internal/app/doc.go`. For **each** of these six POST paths:

* `/worker/register`
* `/worker/heartbeat`
* `/worker/lease`
* `/worker/result`
* `/worker/fail`
* `/admin/credit`

body `{` produces Python **500** text `Internal Server Error`, versus Go **400**
JSON `{"detail":"invalid JSON"}`. Body `[]` produces Python **500** with that
same text, versus Go **400** JSON `{"detail":"invalid body"}`. The harness
prints every difference, classifies it only if both exact bodies/statuses
match these expectations, and exits nonzero on any unexpected difference.

One clear compatibility bug was found and fixed: an untouched ledger returned
Python `[]` but Go `null` at `/admin/ledger/tail`. `Billing.Tail` now supplies a
non-nil empty slice; `TestAdminEmptyLedgerArray` is an HTTP regression test.
An initial summary mismatch was solely `last_used` wall-clock jitter, now
explicitly normalized along with the other documented timestamps.

## Test and binary evidence

On Go 1.26.6 darwin/arm64 / CPython 3.15.0:

* `go test -count=1 -json ./...`: **118 top-level tests + 47 subtests pass**,
  zero failed/skipped tests. Count terminal `pass` events with a `Test` field;
  names without `/` are top-level, names containing `/` are subtests. Package
  terminal events: 11 pass, 2 skip (command/helper packages without tests).
* `go vet ./...`: passes.
* `uv run --frozen pytest -q`: **63 passed**, matching the baseline count.
* The regression fixture needed an actually empty billing database with its
  required disposable prices file; two intermediate test runs failed during
  fixture construction. Final full suite above is green, not those runs.

Reproduce the Linux x86-64 artifact without committing it:

```sh
cd go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 /opt/homebrew/bin/go build \
  -trimpath -ldflags='-s -w -buildid=' -o /tmp/nocturne-shadow-linux ./cmd/nocturne
wc -c /tmp/nocturne-shadow-linux
file /tmp/nocturne-shadow-linux
shasum -a 256 /tmp/nocturne-shadow-linux
```

Observed **11,550,846 bytes**, statically linked, stripped ELF x86-64.
SHA-256 `2ddd857cb703db8ab44c29e34d89fabd61d0e8982e2c1766e512f4859ad689b7`.
A second build with identical flags was byte-identical (`cmp`). This does not
mean the Linux binary was executed on macOS.

Independent final review remains required. This verification is not deployment
approval or an independent review signoff; Python remains the default runtime.

## Security-gate revalidation (2026-10-15)

On the historical production-adapter baseline with security hardening, using official Go 1.26.9:

```
SHADOW cases=126 equal=124 differences=2 intended=2 unexpected=0
```

Only authenticated malformed `/admin/credit` bodies `{` and `[]` retain the
exact Python 500 text versus Go 400 JSON errors described above. Worker malformed
requests now agree; the older twelve-difference result is historical, not the
current expected error count. Authentication occurs before body buffering in Go;
valid worker credentials still undergo the final machine-ID check under lock.
All shadow listeners and state are disposable fixtures, not live service probes.
Set `GO` to the verified compiler path when reproducing this run.
