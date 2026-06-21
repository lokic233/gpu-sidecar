# Lifecycle Hysteresis (corrected)

Source: `internal/core/lifecycle.go`. Tested: `lifecycle_test.go` (table-driven) + `supervisor_test.go`.

## Hard vs soft failure evidence
| Evidence | Class | Transition |
|---|---|---|
| Device gone from enumeration / adapter init failed / runtime says no device / `!GPUVisible` (unclassified) | **HARD** | → OFFLINE immediately |
| One vendor-CLI timeout | SOFT | → DEGRADED; OFFLINE after N consecutive |
| One failed active access probe | SOFT | → DEGRADED; OFFLINE after N consecutive |
| One malformed/short telemetry response | SOFT | → DEGRADED; OFFLINE after N consecutive |

Adapters classify failures: `nvidia.go:classifyNVFailure` (stderr markers like "No devices were
found"/"Unable to determine the device handle" ⇒ hard; timeout/other ⇒ soft) and
`amd.go:classifyAMDFailure` (card absent from rocm-smi output ⇒ hard; timeout/parse ⇒ soft).

## Failure transition table (OfflineFailures=3 default)
```
READY --1 soft fail--> DEGRADED (consecutive_soft_failures=1)
DEGRADED --soft fail--> DEGRADED (=2)
DEGRADED --soft fail--> OFFLINE  (=3, reason OFFLINE_FAILURE_THRESHOLD_REACHED, hard_offline=false)
READY --hard evidence--> OFFLINE (immediate, hard_offline=true)
DEGRADED --1 healthy probe--> READY/BUSY (transient condition cleared; applies immediately)
```

## Recovery transition table
```
OFFLINE --1 healthy probe--> RECOVERING (records rejoin + recovery_duration_ms)
RECOVERING --healthy, hold<RecoveringHoldSec OR streak<RecoveryStreak--> RECOVERING (stay)
RECOVERING --healthy, hold>=5s AND streak>=3--> READY/BUSY (reason RECOVERY_STREAK_MET)
RECOVERING --degrading condition--> DEGRADED
```
A single good probe NEVER takes OFFLINE→READY directly (no flapping). READY↔BUSY changes require
`ConfirmSamples=2` consecutive confirmations (single-sample util spikes don't flip state).

## Configurable parameters (DefaultLifecycleConfig)
| Param | Default | Meaning |
|---|---|---|
| OfflineFailures | 3 | consecutive SOFT failures ⇒ OFFLINE |
| ConfirmSamples | 2 | confirmations for READY↔BUSY |
| RecoveringHoldSec | 5s | min time in RECOVERING |
| RecoveryStreak | 3 | min consecutive healthy probes before promotion |
| DegradedScore | 0.55 | stability below ⇒ DEGRADED |
| BusyUtilPct / BusyMemRatio | 80% / 0.10 | capacity-constrained thresholds |

All exposed per device in `lifecycle.offline_failure_threshold`, `consecutive_soft_failures`,
`healthy_streak`, `recovery_streak_required`, `hard_offline`, `reason_codes`.

## Real-hardware evidence
- H100: one soft failure → DEGRADED; OFFLINE at exactly `soft_failures=3`; recovery via RECOVERING.
- MI350X: identical sequence; OFFLINE reached ~19.5s vs H100 ~15.5s (AMD probe cadence is slower).
