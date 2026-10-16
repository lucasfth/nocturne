# nocturne admin

Vue 3 + TypeScript + Vite, Tailwind CSS v4, and CLI-generated shadcn-vue components. System fonts, neutral surfaces, violet accents, responsive layouts and system-aware dark mode.

## Development

```sh
cd web
~/.bun/bin/bun install --frozen-lockfile
~/.bun/bin/bun run dev
```

Open the printed URL at `/admin/`. API routes under `/admin/summary`, `/admin/machines` `/admin/ledger` and `/admin/config` proxy to `http://127.0.0.1:8702`. The admin secret is stored only in sessionStorage (`s`); real API requests always send a Bearer header. A 401 clears the session.

## Mock mode

```sh
VITE_MOCK=1 ~/.bun/bin/bun run dev --port 5199
```

Open `http://localhost:5199/admin/` and enter any nonempty secret. Fixtures are loaded locally. Add, edit, enable/disable and remove mutate memory; reloading resets the fixtures. No backend required. Mock code is excluded from the normal production bundle.

## Build

```sh
~/.bun/bin/bun run build
~/.bun/bin/bun run preview
```

The build runs `vue-tsc --noEmit`, then outputs `dist/`. Serve its contents at `/admin/`; all generated assets use `/admin/assets/`. Production uses external same-origin JS and CSS, without inline scripts, CDNs or font requests. Apply production CSP at the server; Vite development requires its own development policy for HMR.

`bun.lock` is retained and package versions are exact. Source components are under 250 lines. `SOURCE_FILES.txt` contains the full source file inventory.

## Checks

- Login, logout and 401 handling; summary refresh every 15 seconds.
- Preserved original dashboard formulas, metrics, customer/daily tables, explanatory notes and footer.
- Bootstrap paste parsing, validated worker IDs, inline API error details, reusable edit form and destructive confirmation.
- Accessible shadcn dialogs, menus, tooltips, tables, progress bars and Sonner notifications.

## Browser regression tests

```sh
bun install --frozen-lockfile
bun run build
bun run test:e2e
```

Tests launch installed Google Chrome against the real production bundle with the
Python/Go production CSP. They intercept only same-origin admin API responses,
assert Bearer authentication and request bodies, and exercise add, edit,
credential preservation/rotation, validation, API failures, enable/disable and
cancelled/confirmed removal. No live service, wallet or fleet data is touched.
Install Google Chrome before running (or adjust the local Playwright channel).

Machine management uses the registry independently of overview availability.
Each card exposes Edit machine, Enable/Disable and Remove directly; the overflow
menu remains available. Registry-only/offline entries remain manageable even
when the overview has no matching worker. Stored worker credentials are never
fetched or prefilled. Empty credential fields retain existing values.

`dist/` is ignored and intentionally not committed. Source updates do not replace
a running server's previously built assets. Build on the intended serving host
as a separate, authorized deployment step; this test command does not deploy.

## Admin settings

The Settings tab is available even if overview retrieval fails. Both Python and Go
use `schema_version: 1`, an opaque `revision`, typed `fields`, `values`, `effective`
and `restart_required`. PATCH sends only `{revision, changes}` for changed fields.
There is no `schema_revision` field. Currently only service name (live) and public
concurrency limit (operator restart) are editable. The form displays server field
metadata, saved/effective values, validation, pending saves and restart status.
Credentials, models, payment backends, paths and access control are intentionally
not configurable here. There is no generic JSON editor or restart control.

A stale revision blocks saving until an explicit reload. Reload preserves only
fields the operator changed, takes untouched fields from the latest server view,
and asks for review before a second explicit save. Unavailable/unsupported servers
block saving until a successful reload. Server error bodies are not rendered.
Manual configuration-file edits require an operator restart of the single writer.
Mock mode reports settings as unsupported rather than simulating persistence.

The Chrome suite also starts a real Python Coordinator bound to loopback port
5200, serving `dist/` with production CSP. It uses a private temporary directory,
fake payments, no external energy provider and no maintenance loops. It checks
real settings writes, live/effective values, stale revisions, authorization and
readback after page reload. Install Python dependencies first (`uv sync --locked`
from the repository root). If using an existing isolated Python environment:

```sh
NOCTURNE_TEST_PYTHON=/path/to/venv/bin/python bun run test:e2e
```

The other Chrome cases use controlled API responses for error and conflict states;
all four machine CRUD regressions remain in the same suite. Screenshots are kept
in ignored `test-results/`. The suite also starts the disposable Go benchmark server described below.
No production process restart is performed by this suite. Building/testing does not deploy any running service.

## UI assets and accessibility

The fleet distinguishes initial loading, a genuinely empty registry, and a failed
or stale registry. A failed refresh does not advance the last-successful timestamp.
Disabled machines remain visibly disabled even while offline. Navigation wraps at
320px, includes a keyboard skip link, and retains accessible shadcn/Reka dialogs.

The current PNG favicon files are Nocturne artwork; hashes and selection
provenance are in `FAVICON_PROVENANCE.md`. Vite emits content-hashed files under
the existing `/admin/assets/` route (including when the page is opened at `/admin`
without a trailing slash). Do not serve unhashed favicon files through the immutable asset route.

The shadcn-vue CLI is not an application dependency. Its generated components use
Reka UI directly. Required upstream CSS state variants are retained locally in
`src/shadcn-variants.css`, with the upstream MIT notice in `SHADCN_LICENSE.txt`.
Removing the generator removes its vulnerable transitive toolchain without forced
transitive version overrides. Use an explicitly reviewed CLI version only when
regenerating components, not on application startup.

The browser suite includes favicon byte/MIME checks against the disposable real
Python server and 320px, 390px and 1440px no-overflow, settings-discovery and dialog
keyboard-focus checks. Its screenshots contain synthetic fixtures only and remain
untracked. None of these checks establish live deployment acceptance.

## Benchmark & optimise (Go admin only)

Enabled machine cards open a machine-scoped benchmark panel. The default request
is `{}`: automatic context/parallel/budget requirements remain under collapsed
Advanced / Nerd. The UI displays server-retained history, fixed progress stages,
cancellation, recovery fencing, measured summary and allowlisted provenance.
No percentage, candidate rankings, missing metrics or successful execution are
invented. The API exposes a single measured row, not a candidate comparison log.

Apply requires a separate confirmation and server acceptance. Worker-attested
healthy Apply, verified rollback and recovery-required outcomes are distinct.
Closing the panel stops browser polling, not server work. Polling is serialized,
backs off to 15 seconds (30 on errors), stops after five consecutive failures,
and runs only with outstanding work. Reopen or Reload history reconciles state.

Production benchmark targets and Apply adapters are currently not enabled. The
UI does not hardcode this forever: it consumes the actual Go route responses and
explains unsupported 409 without inventing a job or falling back to a model.
Python gateway/demo mode do not support these endpoints. Same-origin admin Bearer
auth and the server tailnet guard are unchanged; no paths, commands or credentials
are editable in the benchmark panel.

The browser suite launches `tests/run-go-benchmark-server.sh` on loopback 5201.
It builds `tests/go-benchmark-server.go` in a disposable Go module subdirectory
for internal imports, removes those generated sources, and runs against a private
temporary store, fake payments and the actual Go admin/worker routes. Worker
measurements and healthy Apply attestations are explicitly SYNTHETIC TEST DATA.
This validates UI/HTTP/store lifecycle integration, NOT inference performance,
physical restoration or the production adapter. Cases cover completed evidence,
explicit Apply, cancellation, recovery fencing and unsupported start/Apply.
Go 1.26+ and the existing Go dependencies are required for this suite.
