# Documentation Reconciliation (Round 3)

All authoritative-looking claims now match the implementation. Changes:

## Round-1 final_report.md → marked SUPERSEDED HISTORICAL
- Header now reads "⚠️ SUPERSEDED (Round-1 historical)" and points to
  `final_polish/final_polish_report.md` (authoritative) + `final_hardening_report.md`.
- Inline "SIGKILL crash" corrected to "worker disappearance (SIGKILL issued by harness; cause NOT
  inferred by sidecar)".
- Correction banner lists every superseded claim (crash, effective_capacity, readiness, OFFLINE/recovery, security).

## Corrected / weakened claims (repo-wide)
| Old claim | Corrected to |
|---|---|
| "SIGKILL crash detected" / "sidecar independently identified a crash" | "harness issued SIGKILL; sidecar **independently observed worker disappearance** after the measured latency; termination cause NOT inferred" |
| `effective_capacity` (serving-capacity estimate) | `host_capacity_hint`, "transparent host-derived heuristic, NOT equivalent to runtime serving capacity" |
| readiness == GPU visible | host control-plane readiness (collected + not-stalled + ≥1 trustworthy device); per-device `/readyz?device=N`; NOT proof every GPU can serve |
| OFFLINE on one failed probe | hard vs soft hysteresis; soft → DEGRADED → OFFLINE after threshold |
| recovery could exit via DEGRADED | recovery **latched**; releases only on hold + healthy streak |
| unknown disappearance = abnormal (stability penalty) | unknown disappearance **neutral**; only confirmed-abnormal/OOM/rapid-restart penalize |
| default bind `[::]:9095` (all interfaces) | default `127.0.0.1:9095` (loopback-only); explicit override for mesh; WARNING on non-loopback |
| (implied) production security | explicitly NO auth/authz/TLS; trusted-network-only; documented in api_security_notes.md |

## Files updated
- `README.md` — Round-3 banner; loopback default; per-device readiness + aggregate fields; drain POST.
- `artifacts/final_report.md` — superseded banner + inline crash fix.
- `artifacts/api_security_notes.md` — loopback default + explicit override guidance.
- `artifacts/readiness_semantics.md` — Round-3 aggregate + per-device section.
- `artifacts/worker_event_semantics.md` — Round-3 neutral-disappearance + bounded-history section.
- Round-1 schema docs (`normalized_schema.md`, `lifecycle_state_machine.md`, `stability_score.md`) carry
  Round-2 supersede banners (still accurate after Round 3; recovery-latch detail is in `lifecycle_hysteresis.md`).

## Raw evidence preserved (not edited)
Round-1/Round-2 raw JSON under `artifacts/{h100_raw,mi350x_raw,mesh_raw,validation_round_2}/` still
contains the historical `effective_capacity` field and old metrics — intentionally preserved as
provenance of what was measured at the time. The CURRENT contract is in `final_polish/` + live `/v1/status`.
