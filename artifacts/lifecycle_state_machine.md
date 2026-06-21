# Lifecycle State Machine

> **Superseded in part by Round 2 (correctness).** See `final_hardening_report.md`, `readiness_semantics.md`, `lifecycle_hysteresis.md`, `worker_event_semantics.md`, `capacity_semantics.md`. Notably: `effective_capacity`→`host_capacity_hint` (heuristic); OFFLINE now uses hard/soft hysteresis; lifecycle exposes `reason_codes`.

Source: `internal/core/lifecycle.go`. Tested in `lifecycle_test.go`. Uses **monotonic time**
for all durations (clock-skew safe) and **hysteresis** to prevent flapping.

## States
| State | Meaning |
|---|---|
| UNKNOWN | initial, before first successful classification |
| READY | GPU visible, accessible, healthy, capacity available |
| BUSY | healthy but capacity-constrained (high util or low free mem) |
| DEGRADED | reachable but unhealthy: low stability score OR new uncorrectable/RAS errors |
| DRAINING | operator-initiated graceful drain (via `/v1/drain?device=N&on=true`) |
| OFFLINE | GPU not visible/accessible OR ≥ OfflineFailures consecutive probe failures |
| RECOVERING | transient state after returning from OFFLINE; held before promotion |

## Transitions
```diagram
                  ┌──────────────────────────────────────────────┐
                  │                                               │
   UNKNOWN ──► READY ◄──► BUSY                                    │
                  │  ▲      │                                     │
        low score │  │      │ low score / new errors             │
        or errors ▼  │      ▼                                     │
              DEGRADED ─────┘                                     │
                  │                                               │
   any state ─────┼── probe fail ×N / not visible ──► OFFLINE ────┘
                  │                                       │
   operator ──► DRAINING                                  │ probe recovers
                                                          ▼
                                              RECOVERING ── held ≥5s healthy ──► READY/BUSY
                                                  │
                                                  └── re-degrade ──► DEGRADED
```

## Anti-flap rules (defaults, `DefaultLifecycleConfig`)
- **Failure path is immediate**: any OFFLINE target applies in one sample (fast safety).
- **Healthy transitions need confirmation**: `ConfirmSamples=2` consecutive samples to change
  between READY/BUSY (a single-sample util spike does NOT flip state — proven by `TestLifecycleNoFlapping`).
- **DEGRADED and DRAINING apply fast** (1 confirmation) because they are safety-relevant.
- **Recovery is gated**: after OFFLINE, the device is forced through RECOVERING and held for
  `RecoveringHoldSec=5s` of healthy probes before it can return to READY/BUSY. This prevents an
  OFFLINE↔READY oscillation and gives the stability score time to reflect the disruption.

## Thresholds (defaults)
| Param | Value | Used for |
|---|---|---|
| BusyUtilPct | 80% | util ≥ ⇒ BUSY candidate |
| BusyMemRatio | 0.10 | free-mem-ratio ≤ ⇒ BUSY candidate |
| DegradedScore | 0.55 | stability below ⇒ DEGRADED |
| OfflineFailures | 3 | consecutive probe failures ⇒ OFFLINE |
| ConfirmSamples | 2 | hysteresis for healthy transitions |
| RecoveringHoldSec | 5s | min healthy hold before leaving RECOVERING |

State is never derived from one instantaneous utilization sample alone — it combines probe
success, accessibility, consecutive-failure history, capacity, the smoothed stability score,
and error counters.
