# Offline Go CPU hotpath scaling

Historical measurements on Darwin arm64 / Apple M1 compare the pre-optimization baseline.
No production service or live worker was exercised. SQLite indexing remains
separate and unchanged.

## Changes and invariants

- Nonstream worker text/reasoning use amortized `strings.Builder` growth.
  Jobs remain pointer-owned, never copied after writing; retry resets both
  builders and raw rune counts. Public chat also uses builders for nonstream
  output and does not retain unused streamed text/reasoning.
- Interactive and bulk queued counts change under the existing coordinator
  lock, with every status transition. Cancel, retry, failure and cleanup retain
  scan-equivalent counts. Retained terminal jobs no longer affect pending cost.
- Worker heartbeat/lease calls `Energy.Serves` with that worker's current job
  count instead of snapshotting/sorting the fleet. It retains `Window`,
  `policy.Eligible`, the energy monotonic clock and existing bulk slot logic.
  Full sorted `State` is unchanged for administrative output. Price/tier work
  is omitted only where it never participated in the serving decision.
- Billing callbacks, registry authentication and lock protocols are unchanged.

## Measurements

`go test ./internal/app -run '^$' -bench BenchmarkHotpath -benchtime=100ms -count=3`
uses ReportAllocs at 256, 1024 and 4096. Values below are medians of three runs;
bytes and allocations are their own medians. Baseline uses the same fixtures
with old pending scan, old string length and full State in the serving loop.
Pending/serving measure N decisions at population N, not one request. Pending
is a direct internal microbenchmark without lock or HTTP costs. Worker result
includes result validation and response serialization, with N 16-byte chunks.
Serving uses no schedule and a fixed wall clock; configured timezone/window
cost still exists and is independent of fleet population.

| Benchmark | N | Before ns/op | After ns/op | Before B/op | After B/op | Before allocs/op | After allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|
| Pending batch | 256 | 489226 | 89.24 | 0 | 0 | 0 | 0 |
| Pending batch | 1024 | 8975924 | 329.2 | 0 | 0 | 0 | 0 |
| Pending batch | 4096 | 147554667 | 1289 | 0 | 0 | 0 | 0 |
| Worker result | 256 | 169275 | 77632 | 601949 | 55250 | 2052 | 1807 |
| Worker result | 1024 | 1785379 | 310392 | 9020758 | 216376 | 8207 | 7188 |
| Worker result | 4096 | 24493142 | 1242505 | 145055784 | 881608 | 32973 | 28698 |
| Serving batch | 256 | 14355631 | 7587 | 27311793 | 0 | 72234 | 0 |
| Serving batch | 1024 | 242526459 | 31761 | 463948224 | 0 | 1086094 | 0 |
| Serving batch | 4096 | 4091783875 | 129788 | 7442219008 | 0 | 17051022 | 0 |

At 4096 chunks, 65536 output bytes require 0.882 MB allocated rather than
145.056 MB, and 1.243 ms rather than 24.493 ms. These are measured allocation
totals, not resident memory or production throughput predictions.

## Verification

- Full `go test -count=1 ./...` and `go vet ./...` pass.
- `go test -race -count=1 ./internal/app ./internal/energy` passes.
- Python pinned baseline and current: 155 passed each.
- Offline HTTP shadow: 126 cases, 114 equal, 12 intended differences,
  zero unexpected differences.
- Added queued-counter scan invariants across both lanes, cancellation,
  lease/retry/second failure/final/timeout/retention cleanup; 4096-chunk Unicode
  accumulation and final-accounting ordering; exhaustive representative
  State/Serves parity across readiness, battery thresholds, active jobs,
  thermal states, user activity, 45-second boundary and schedule boundaries.
- Existing public stream think stripping, terminal-accounting failures,
  slow-consumer and bulk scheduling tests remain green.
- Independent read-only Pi review reported no concrete bugs. Reviewer did
  not run tests; execution evidence above is separate.
