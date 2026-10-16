# Contributing to Nocturne

Nocturne is a general-purpose, self-hostable inference gateway, not an
organization-specific automation suite. Keep examples portable and preserve
existing authorship and the AGPL-3.0-or-later license. `CLAUDE.md` is a relative
symlink to this file, so humans and coding agents share one set of instructions.

## Architecture and boundaries

- `gateway/`: default Python 3.12+ coordinator, FastAPI public/worker/admin apps,
  SQLite billing, machine registry, energy policy, FX and invoice-only adapters.
- `go/`: opt-in Go 1.26+ implementation under evaluation. Python remains the
  default and rollback runtime. Explicitly document feature/parity differences.
- `worker/`: Python Mac agent and privileged, owner-consented bootstrap/launchd
  helpers. `simworker/` provides fake workers; no Mac provisioning needed for tests.
- `web/`: Vue 3, TypeScript and Vite admin SPA, built separately into `web/dist`.
- `tools/`: explicit offline comparisons and restricted tunnel-key synchronization.

All listeners bind loopback: public 8700, worker 8701, admin 8702. Never expose
worker/admin through the public proxy. Tailscale identity headers are trusted
only from the local trusted proxy, never arbitrary client-supplied headers.
Admin APIs require Bearer credentials even when identity headers are present.
Do not weaken auth, browser-origin checks, request limits, billing idempotency,
worker safety policy or SSH forwarding restrictions to make a test pass.

## Canonical local commands

```sh
uv sync --locked
uv run pytest -q
(cd go && go test ./... && go vet ./...)
make go-build
# Frontend is not embedded or automatically built by either gateway:
(cd web && bun install --frozen-lockfile && bun run build)
# Run only with disposable config/data and explicit lightning.backend=fake:
SERVICE_ROOT=. uv run python -m gateway
```

See README quick start for fixture initialization and authenticated smoke checks.
For isolated HTTP parity use the commands in `docs/shadow-comparison.md`.
Tests must use temporary directories and fake payment backends. Never point tests,
comparison tools, startup recovery or two concurrent coordinators at real data.
Do not install services, provision workers, migrate real ledgers or deploy without
explicit operator approval. Avoid wildcard service activation.

## Configuration and secrets

Start from `config.example.json`; explicitly select the payment backend. Secret
files and runtime data must stay untracked. Never print tokens, credentials,
wallet seed words, config contents containing secrets or full secret-bearing
errors. Keep runtime directories private (0700), files 0600. Use placeholders in
docs; do not include private host paths, identities or infrastructure addresses.

Python's `/admin/config` exposes a deliberately small typed nonsecret allowlist.
Preserve unknown config extensions on writes but never return them. Settings
must say whether they apply live or require restart. Do not claim Go parity or
admin UI coverage before it exists. Credential, command, path, backend and
access-control editing require a separate security design, not generic JSON CRUD.
The config writer is single-process: external/manual edits require restart;
never share its config file between running coordinators.

## Contribution and review

Use focused branches and conventional commits; explain behavior, compatibility,
security impact and tests. Review authorization and error redaction, persistence
(file + directory fsync, atomic replacement), concurrent updates, request limits
and no unintended outbound calls. Add regression tests for changed behavior.
Enable `git config core.hooksPath .githooks` and install gitleaks before committing.
Never commit config.json, data/, keys, databases, logs, generated web/dist or Go
binaries. Keep runtime and frontend contracts documented. Do not overwrite
unrelated changes. Report untested paths honestly; build success is not a live
production verification. Follow the operator's explicit review/deployment policy.
