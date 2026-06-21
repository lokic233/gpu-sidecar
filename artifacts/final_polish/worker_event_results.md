# Worker-Event Semantics Results (Round 3)

## Neutral disappearance (default)
`WORKER_DISAPPEARED` with `termination_cause=unknown` is NEUTRAL: it does not reduce stability.
A disappearance (compute-process count / memory delta) cannot distinguish graceful shutdown,
scale-down, rolling replacement, SIGTERM, SIGKILL, crash, OOM, or eviction.

Score inputs renamed to reflect actual evidence (internal/core/stability.go):
| Input | Penalizes stability? | Populated by host sidecar? |
|---|---|---|
| `WorkerDisappearancesObserved` | NO (neutral) | yes (observability) |
| `ConfirmedAbnormalWorkerExits` | yes | no (needs supervised/cgroup/runtime evidence) |
| `ConfirmedOOMEvents` | yes (strong) | no (needs OOM evidence source) |
| `RapidRestartEvents` | yes | yes (disappear→reappear within 10s) |

## Tests (internal/core/worker_stability_test.go)
- `TestStability_UnknownDisappearanceNeutral` — 1 unknown disappearance: score unchanged.
- `TestStability_ManyUnknownDisappearancesNeutral` — 20 graceful disappearances: score unchanged.
- `TestStability_ConfirmedAbnormalReducesScore` — confirmed abnormal exits: score drops.
- `TestStability_ConfirmedOOMReducesScore` — confirmed OOM: score drops.
- `TestStability_RapidRestartReducesScore` — rapid restart loop: score drops.
- `TestStability_NeutralPlusConfirmed` — neutral disappearances add nothing beyond the confirmed penalty.

## Real-hardware evidence (H100 dev6)
A workload ran and exited (graceful). Events: `WORKER_STARTED` then `WORKER_DISAPPEARED cause=unknown`
("compute process count decreased (cause NOT observable from host signals — neutral)"). Device
stability stayed ~0.99 across the disappearance — no penalty applied. See
`h100_smoke/neutral_disappearance_events.txt`.
