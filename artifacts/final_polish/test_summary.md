# Test Summary (Round 3)

Baseline a24e19c had 80 tests. Round 3 adds recovery-latch, worker-neutrality, bounded-history,
loopback-config, and multi-GPU/per-device readiness tests → **112 tests total**, all passing.
`go vet ./...` clean. `go test -race ./...` clean.

## Per-package test functions (112 total)
| Package | Tests | Focus |
|---|---|---|
| internal/core | 31 | model, lifecycle hysteresis, **recovery latch (5)**, stability decay/recovery, **worker neutrality (6)**, history, parsers |
| internal/engine | 27 | supervisor (mock adapter + injectable clock), **bounded worker history (6)**, **readiness aggregate (6)**, concurrent drain/poll race |
| internal/api | 21 | httptest endpoints, drain method/validation/idempotency, **multi-GPU + per-device readiness (7)**, concurrency, unsupported-field serialization |
| internal/adapters | 15 | nvidia/amd parsers, failure classifiers, fault-inject (hard/soft), missing-tool/timeout/malformed |
| internal/exec | 6 | timeout, missing command, non-zero exit, bounded output |
| internal/config | 2 | **loopback default**, IsLoopback matrix |
| cmd/collector | 10 | healthy/unreachable/stale/malformed/slow/mixed-vendor/duplicate-id |

## Commands
```
go test ./...        # all pass
go test -race ./...  # all pass (clean — incl. concurrent drain vs poll)
go vet ./...         # clean
```

## Key tests added this round
- Recovery latch (core): `TestRecoveryLatch_LowStabilityDoesNotBypass`, `_BusyDoesNotBypass`,
  `_SoftFailuresReturnOffline`, `_HardFailureImmediateOffline`, `_StreakResetsOnInterruption`.
- Worker neutrality (core): `TestStability_UnknownDisappearanceNeutral`, `_ManyUnknownDisappearancesNeutral`,
  `_ConfirmedAbnormalReducesScore`, `_ConfirmedOOMReducesScore`, `_RapidRestartReducesScore`, `_NeutralPlusConfirmed`.
- Bounded history (engine): `TestWorkerEventLog_BoundedBySize`, `_BoundedByAge`, `_OldEventsDontAffectScoring`,
  `_RecentRetained`, `_RapidRestartDetection`, `_NoOrderingErrors`.
- Loopback (config): `TestDefaultIsLoopbackOnly`, `TestIsLoopback`.
- Readiness (api+engine): all-ready / some-ready / none / per-device 200/503/404 / inaccessible /
  invalid / stale / collector-stalled / first-collection.
