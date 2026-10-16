# Local benchmark and installer gate

Stage **all** of `agent.py`, `macos_sensors.py`, `__init__.py`,
`benchmark_setup.py`, and `model_benchmark.py` from `worker/` into
`/tmp/nocturne-pkg` before invoking `worker/install-services.sh`, even for a skip.
The installer checks module presence and gates before service/config mutation.
The older README staging recipe is insufficient; updating that parent document
is outside this benchmark-only change.

Set `NOCTURNE_BENCHMARK_CATALOG` to a trusted local catalog. No models are
installed, switched, downloaded or relabeled by the benchmark. For this fixed
installer, the catalog must have exactly one candidate: the existing
`/opt/nocturne/models/Ornith-1.5-9B-Q4_K_M.gguf`, declared `Q4_K_M`, and server
`/opt/nocturne/bin/llama-server`. It must explicitly include `Q4_K_M` in
`allowed_quantizations` and exactly one profile:

```json
{"context": 32768, "parallel": 2, "spec_n": 2}
```

This opt-in accepts a **quality tradeoff**, not evidence that Q4 is equivalent
to higher precision. Default policy excludes Q4. Start with the example catalog,
replace its candidates/server with the above existing local files and explicitly
add Q4 to the allowlist. Unsupported installer configurations refuse **before
inference**, rather than spending the budget on an un-installable recommendation.
The gate requires completed selection, matching runtime settings and verified
cleanup. It never applies a recommendation. An explicit
`NOCTURNE_SKIP_BENCHMARK=1` records `unbenchmarked`, policy not evaluated, and the
fixed installed settings; it is not a pass or safety assertion.

## Inventory without a catalog

```sh
python -m worker.model_benchmark --probe-only --output ./inventory.json
python -m worker.model_benchmark --catalog ./catalog.json --output ./dry-run.json
# Only after owner approval and exclusive standalone use:
python -m worker.model_benchmark --catalog ./catalog.json --output ./results.json --execute
```

Probe-only needs no model, quantization declaration or backend. It runs only
read-only OS sensors, **no server calls**, preserving unsafe baselines (including
pressure 2) with blockers. Catalog dry-run additionally calls the trusted binary's
`--version`; neither mode performs inference. Reports omit raw paths, process
arguments, errors, hostnames and credentials. They use unique, no-clobber atomic
publication, mode 0600, file/directory fsync. Use a private output directory.
`--expected-hostname` optionally guards execution without publishing identities.

## Policy and runtime

The example defaults to 32768 context **per request**, parallel 2, 600 seconds,
128 output tokens, two measured repeats. These are conservative choices, not
hardware evidence: a full run may honestly time out within 600 seconds.
All benchmark runtimes mirror fixed installer flags:

```
-ctk q8_0 -ctv q8_0 -fa on -ngl 999 --reasoning-format deepseek
--spec-type draft-mtp --spec-draft-n-max 2
```

Only `spec_n` is configurable per profile, integer 0–3, default 2; installation
requires 2. No arbitrary runtime flags/overrides are accepted. Thinking is **not
disabled**. Unsupported binaries/models fail or time out, never silently fall
back to different settings. Context passed to the binary is per-slot context
multiplied by parallelism. The backend must implement `/health`, `/apply-template`,
`/tokenize`, `/completion`, API keys, explicit slots and timing fields.

Catalog limits: 1–3 candidates, 1–6 profiles, context 4096–32768 subject to
`min_context` (default 8192), parallel 1–2, runtime 10–600 seconds, output 1–256,
repeats 2–5. Defaults allow Q5_K_M, Q6_K, Q8_0, F16 and BF16; Q4_K_M is an
explicit allowlist opt-in. Unknown fields, duplicate keys, nonfinite numbers,
argument overrides, and other quantizations refuse. Paths resolve relative to
the catalog. Only existing local executables and GGUF magic are checked.
**Parameter counts and quantization metadata are declared, not verified.**
Never relabel a file to pass policy. No full GGUF tensor parser is included.

Disk floor is at least 5 GiB, headroom at least 20%, throughput floors positive,
swap growth allowance 0–1024 MiB (example zero). A preliminary file-size/RAM
filter is only plausibility, not runtime fit. All requested profiles must pass
quality, throughput and safety; largest declared parameter count wins among
passing catalog candidates, not globally. Timeout/interruption/cleanup failure
suppresses selection. No passing candidate returns nonzero.

## Fixtures and limitations

Every prompt is built as system/user messages and passed through `/apply-template`
with assistant generation prefix, then tokenized in full. No template, role,
needle or suffix tokens are cut. Distractor record count is adjusted to occupy
within 96 tokens of `context - output_allowance - 8`. Short speed prompts target
1024; long speed and every quality request target near-full context. Requests
run at configured parallelism, with cache reuse disabled, warmup plus measured
repeats. Returned prompt counts must match actual token counts.

Early code and middle color/count facts precede distinct distractor records;
questions occur at the end. Mandatory fixtures check exact grounded retrieval,
output formatting (lowercase/no punctuation), and exact JSON keys, grounded
values and types. A separate early call record says caller Mira calls supplier
Sol, outbound from Mira; unrelated Nia/Theo calls distract. The final instruction
explicitly forbids inverting roles. The exact expected JSON is
`{"caller":"Mira","recipient":"Sol","direction":"outbound"}`. Schema-valid
reversed roles fail; this is call direction, not output formatting. Schema compliance alone cannot pass. The system role
directs use of records as data. No constrained schema or thinking-disable switch
is used. Small output budgets, reasoning wrappers or chat-template differences
can cause legitimate failures; they are not stripped away to inflate results.

These synthetic fixtures are narrow, repetitive, not adversarial and not a
universal intelligence/application-quality test. `occupied_context` describes
occupancy, **not universally usable context**. Passing provides only fixture
retrieval-span evidence. Timing, RSS, headroom, swap and verdicts are reported.
Live llama.cpp/macOS compatibility remains unverified.

## Safety and cleanup

Admission requires AC, battery >=50%, idle >=120 seconds, nominal thermal state,
normal pressure (1), memory/disk floors and no competing worker/server services
or processes. Missing sensors fail closed (including battery-less desktops).
Absent optional launchd GUI/user domains are accepted only with status 64/113
and the domain-not-found diagnostic, or status 125 with the exact stripped
macOS diagnostic `Could not print domain: 125: Domain does not support specified action`.
This exception applies only to optional domains, never the system domain;
other diagnostics/statuses, including permission errors, fail closed.

Process detection combines `ps comm` executable metadata with `ps args`; it never
shell-parses arbitrary command strings. Paths containing spaces remain intact.
Ambiguous shell references conservatively block; process snapshot races refuse,
except the second `ps` invocation itself (`ps` or `/bin/ps` at an executable
boundary), which is necessarily absent from the first snapshot.
Snapshots have a **whole-snapshot 3-second timeout**, with bounded child commands;
monitoring sleeps 0.5 seconds after each snapshot, not fixed 0.5-second sampling.
Thus failure detection may take about 3.5 seconds plus owned-process teardown
(TERM then KILL), subject to OS scheduling. A slow snapshot aborts inference.

There is no drain, unload, restoration or atomic admission fence. Independent
services can start after any snapshot; execution requires owner-guaranteed
exclusive standalone use. A per-user lock does not exclude other users.
SIGINT/SIGTERM use the same session handler in both entry points, triggering
owned-process-group cleanup and interruption artifacts. Temporary directories
are removed and a final safety snapshot is attempted. SIGKILL/power loss cannot
be recovered by finally blocks. No report establishes production readiness.

## Admin integration status

The Go API now includes durable reconciliation and a real installed worker
execution path, disabled until explicit trusted installation and operator startup
binding. See [current contract](go-admin-benchmark.md) and
[installation](production-benchmark-install.md). Automatic bounded protected local
inventory and descriptor-bound model loading apply only to the installed runner.
The standalone CLI above retains its declaration-based catalog and standalone
exclusive-use requirements. Production Apply is still disabled. Cleanroom tests
are not live launchd or inference verification.

## Tests

```sh
uv run pytest -q tests/test_model_benchmark.py tests/test_benchmark_setup.py
```

Tests use fake probes/backends, temporary files and harmless local children;
no inference, service activation, remote access or real ledgers.
