# Final Polish Report (Round 3) — AUTHORITATIVE CURRENT REPORT

This is the authoritative current report for github.com/lokic233/gpu-sidecar. Round-1
`artifacts/final_report.md` is superseded historical; Round-2 `artifacts/final_hardening_report.md`
remains accurate for the correctness-hardening pass.

## 1. Verdict

**FINAL_POLISH_COMPLETE_WITH_PARTIAL_HARDWARE_SMOKE**

All six target issues are fixed with failing-test-first reproduction, the full suite passes
(112 tests, `go vet` clean, `go test -race` clean), and lightweight smoke validation on BOTH real
nodes (H100 devgpu014, MI350X devgpu499) confirms the new behavior with no vendor regression. It is
"partial hardware smoke" by design: per the brief, no destructive/stress hardware experiments were
rerun — only safe state-machine/API/config smoke checks.

## 2. Recovery latch fix
- **Previous bypass:** RECOVERING could drop to DEGRADED (low stability) or be treated as BUSY, and the
  next healthy probe used the DEGRADED→READY fast path — bypassing hold + healthy-streak.
  Path `OFFLINE→RECOVERING→DEGRADED→READY`.
- **Corrected invariant:** explicit latch (`recovery_latched`, `recovery_started_at`,
  `recovery_healthy_streak`). Once OFFLINE, recovery is latched; low stability / high util / transient
  soft degradation keep the device in RECOVERING (annotated via `reason_codes`) and CANNOT release it.
  Release requires BOTH `RecoveringHoldSec` (5s) AND `RecoveryStreak` (3 consecutive healthy). A soft
  failure resets the streak (stays latched); threshold soft failures or hard evidence → OFFLINE + re-latch.
  No path through any intermediate state bypasses hysteresis (5 table-driven tests + real-HW traces).

## 3. Worker-event semantics
- `WORKER_DISAPPEARED` (cause=unknown) is **neutral**: it does not lower stability. A count/memory delta
  can't distinguish graceful stop / scale-down / rolling replacement / SIGTERM / SIGKILL / crash / OOM / eviction.
- Stability is penalized ONLY by `ConfirmedAbnormalWorkerExits`, `ConfirmedOOMEvents` (strong), or
  `RapidRestartEvents` (disappear→reappear within 10s — observable by the host). The first two require a
  future supervised/cgroup/runtime evidence source and are 0 today; the host populates the neutral
  observation count and rapid-restart detection. (6 tests + real-HW: stability held ~0.99 across a disappearance.)

## 4. Bounded history
`workerEventLog` bounds disappearance/appearance timestamps by BOTH max-age (window pruning, default =
stability window) AND max-size (ring cap = event ring capacity). Old events are removed (not merely
ignored) so memory stays bounded over long runs and scoring uses only the recent window. (6 tests
incl. 10k synthetic events: bounded, age-out, recent retained, monotonic ordering.)

## 5. Readiness contract
- `/healthz`: process alive.
- Host `/readyz`: control-plane readiness — collected + not stalled + ≥1 trustworthy device. 200/503.
  Exposes `control_plane_ready`, `any_device_ready`, `all_devices_ready`, `ready_device_count`,
  `total_device_count`, plus per-device `details[]`. Explicitly NOT proof every GPU can serve traffic.
  Backward-compat aliases (`ready`, `ready_devices`, `total_devices`) retained.
- Per-device `/readyz?device=N`: 200 ready / 503 not-ready (with reasons) / 404 unmanaged.
- Partial multi-GPU failure is unambiguous via `any/all_devices_ready`. (13 tests across api + engine.)

## 6. Security default
Default bind is **`127.0.0.1:9095` (loopback-only)** because `/v1/drain` is an unauthenticated mutation.
Remote/mesh exposure requires an explicit `--listen` override on a trusted network; a WARNING is logged
for any non-loopback bind. No authentication, authorization, or transport security is implemented or
claimed. (`config.IsLoopback` + 2 tests; real-HW: WARNING observed on both nodes when bound to mesh.)

## 7. Documentation reconciliation
See `documentation_reconciliation.md`. Round-1 report marked SUPERSEDED; crash→disappearance,
effective_capacity→host_capacity_hint, readiness, OFFLINE/recovery, and security claims corrected
across README + artifacts. Raw historical evidence preserved unedited as provenance.

## 8. Tests
112 tests; `go test ./...` all pass; `go test -race ./...` clean; `go vet ./...` clean. See `test_summary.md`.

## 9. H100 smoke validation (devgpu014)
- Recovery latch: OFFLINE(latched)→RECOVERING streak 1→2→3→4→READY at RECOVERY_STREAK_MET (`h100_smoke/recovery_latch_trace.json`).
- Neutral disappearance: workload exit → WORKER_DISAPPEARED cause=unknown; stability held ~0.99 (`h100_smoke/neutral_disappearance_events.txt`).
- Readiness: host 4/4 ready, all flags true; `?device=4`→200; `?device=99`→404 (`h100_smoke/`).
- Drain POST changed:true/false idempotent; non-loopback WARNING emitted.
- Vendor telemetry intact: temp 39C, ECC=0, CUDA, 102GB free; capacity heuristic; runtime serving cap unsupported.
- Anomaly: none functional. Background-launch produced occasional duplicate processes (CLI artifact, not a sidecar bug); one instance always served correctly.

## 10. MI350X smoke validation (devgpu499)
- Recovery latch: OFFLINE(latched, ~8s)→RECOVERING streak 1→2→3→READY at RECOVERY_STREAK_MET (`mi350x_smoke/recovery_latch_trace.json`).
- Readiness: host 6/6 ready; `?device=5`→200; `?device=99`→404; GET /v1/drain→405; drain POST changed:true.
- Vendor telemetry intact: junction 56C, power 257W, RAS=0, 308GB free; power_limit unsupported (AMD); capacity heuristic.
- Anomaly: AMD's slower rocm-smi cadence (~220ms/probe × 6 devices) lengthens the OFFLINE→RECOVERING edge vs H100, but the latch invariant holds identically. Per-card proc attribution remains unreliable (unchanged AMD limitation — worker detection leans on memory-delta).

## 11. Remaining limitations
- **No confirmed crash/OOM source:** `ConfirmedAbnormalWorkerExits`/`ConfirmedOOMEvents` are wired but
  always 0 without a supervised-process/cgroup/runtime integration. The host sidecar still cannot prove cause.
- **AMD worker attribution:** `rocm-smi --showpids` per-card mapping unreliable; AMD worker events rely on memory-delta.
- **No runtime serving capacity:** `host_capacity_hint` is a heuristic; true serving capacity (queue depth,
  KV-cache, TTFT/TPOT) needs a runtime plugin (interface only, not implemented).
- **Security:** unauthenticated/unencrypted API; loopback-default + trusted-mesh only. No mTLS/authz/audit-sink.
- **Detection latency is poll-bounded** and ~2-3× slower on AMD due to rocm-smi cost.
- **Single mutation endpoint** (`/v1/drain`) records events in-memory only (bounded ring), not a durable audit log.

## 12. Recommended next step
**Begin experimental router integration.** The host-truth contract is now conservative and internally
consistent: readiness is unambiguous (control-plane vs per-device), recovery cannot flap or bypass
hysteresis, capacity is clearly heuristic, and worker disappearance is neutral. A router can safely
consume `lifecycle_state` + `reason_codes` + per-device `/readyz` + `host_capacity_hint` (as a soft
prior) WITHOUT misinterpreting them. The highest-value follow-on is a thin experimental router that
reads these signals (read-only) to validate the contract end-to-end before investing in a runtime
serving-capacity plugin.
