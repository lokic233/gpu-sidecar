# Final Hardening Report (Round 2 — Correctness)

## 1. Verdict

**CORRECTNESS_HARDENED_AND_REVALIDATED**

All six target correctness issues were fixed, covered by tests (32 → 76, race-clean), and
re-validated on the real H100 (devgpu014) and real MI350X (devgpu499) nodes. A previously-latent
data race (drain vs poll loop) was discovered by the new `-race` tests and fixed. Every weakened
claim is reconciled in the docs and the worker-event taxonomy now separates observation from inference.

## 2. Bugs fixed

### B1 — /readyz conflated readiness with GPU visibility
- **Previous:** ready if ANY device `GPUVisible` (ignored access failures, staleness, OFFLINE).
- **Correct:** per-device contract (collected + visible + accessible + last-probe-ok + fresh + not
  OFFLINE + collector-not-stalled); 200/503 + structured `details[]`.
- **Code:** `internal/engine/supervisor.go` (Readiness, collectorStalled), `internal/api/server.go`.
- **Tests:** supervisor (5 readiness tests), api (ReadyzHealthy/ReadyzInaccessible).
- **Evidence:** single-device H100 dev7 hard fault → **HTTP 503** with reasons incl. LIFECYCLE_OFFLINE.

### B2 — OFFLINE had no soft-failure hysteresis
- **Previous:** one failed access probe → immediate OFFLINE.
- **Correct:** hard evidence → immediate OFFLINE; soft failures → DEGRADED then OFFLINE only after
  `OfflineFailures` (3) consecutive; recovery gated by hold (5s) + healthy streak (3).
- **Code:** `internal/core/lifecycle.go`, adapter classifiers in `nvidia.go`/`amd.go`, `model.go` ProbeFailure.
- **Tests:** lifecycle table-driven (6 sequences) + supervisor (soft/hard/recovery).
- **Evidence:** H100 OFFLINE at exactly soft_failures=3; MI350X same at soft_failures=3.

### B3 — worker disappearance could imply a crash
- **Previous:** `WORKER_STOP` on count decrease; dead `ProcessCrashes` stability input; docs said "crash detected".
- **Correct:** `WORKER_DISAPPEARED` with `termination_cause=unknown` + evidence map; no crash claimed;
  `ProcessCrashes`→`AbnormalDisappearances`.
- **Code:** `supervisor.go:detectWorkers`, `model.go` taxonomy, `stability.go`.
- **Tests:** supervisor WorkerStartAndDisappearEvents (asserts cause=unknown, no crash event).
- **Evidence:** H100 `WORKER_DISAPPEARED cause=unknown evidence={mem_released=22023241728, prev=1, cur=0}`.

### B4 — effective_capacity implied serving capacity
- **Previous:** `effective_capacity` "serving-capacity estimate".
- **Correct:** `host_capacity_hint` + `capacity_semantics=heuristic_host_derived` + components +
  `runtime_serving_capacity_supported=false` + optional runtime plugin struct.
- **Code:** `model.go` (CapacityHint, RuntimeServingCapacity), `supervisor.go:computeCapacityHint`, metrics rename.
- **Tests:** supervisor + api assert semantics + runtime-unsupported; metrics test asserts old name gone.
- **Evidence:** `/v1/status` shows heuristic label + components + runtime unsupported on both vendors.

### B5 — drain mutated state via unauthenticated GET
- **Previous:** `GET /v1/drain?...` mutated; errors ignored.
- **Correct:** POST/PUT only (GET→405), validated fields, idempotent (`changed`), records event with source.
- **Code:** `api/server.go:drain`, `supervisor.go:SetDraining` + `lifecycle.go:SetDrainingChecked`.
- **Tests:** api (RejectsGET=405, PostValid, MissingFields=400, UnknownDevice=404, Idempotent).
- **Evidence:** both nodes: POST changed:true→false→true; **GET → 405**.

### B6 — DATA RACE on drain vs poll loop (found during hardening)
- **Found by:** new `TestSupervisor_ConcurrentDrainAndPoll` under `-race` — `SetDraining` and the poll
  loop's `Step()` raced on `LifecycleMachine.draining`/`state` (unsynchronized). This also explained
  an observed `changed:false` drain anomaly on MI350X.
- **Correct:** added `sync.Mutex` to `LifecycleMachine` (guards State/SetDraining/Draining/Step/Info)
  + atomic `SetDrainingChecked` (check-and-set under one lock).
- **Tests:** ConcurrentDrainAndPoll + full suite under `-race` passes.
- **Evidence:** post-fix, real-hardware drain idempotency correct on both nodes.

## 3. Readiness semantics
See `readiness_semantics.md`. `/healthz`=process alive; `/readyz`=can inspect trustworthily+freshly
(collected+visible+accessible+last-probe-ok+fresh+not-OFFLINE+collector-healthy); `lifecycle_state`=
traffic decision. DEGRADED/BUSY/DRAINING/RECOVERING are ready-for-inspection; OFFLINE is not.

## 4. Lifecycle semantics
See `lifecycle_hysteresis.md` for full tables. Hard→immediate OFFLINE; soft→DEGRADED→(threshold)→OFFLINE;
recovery OFFLINE→RECOVERING→(hold+streak)→READY. No OFFLINE↔READY or READY↔BUSY flapping.

## 5. Worker-event semantics
See `worker_event_semantics.md`. Host sidecar emits only OBSERVED/STARTED/DISAPPEARED (cause=unknown).
CRASH/OOM confirmed events require direct evidence (supervised exit / cgroup / runtime / kernel) — not
available to a pure host sidecar. SIGKILL experiments now say "harness issued SIGKILL; sidecar detected
disappearance after X ms" — never "sidecar identified a crash".

## 6. Capacity semantics
See `capacity_semantics.md`. `host_capacity_hint` is a heuristic (free_mem × util_headroom × stability)
with exposed components; `runtime_serving_capacity_supported=false`. True serving capacity (queue depth,
KV-cache, TTFT/TPOT) needs a runtime plugin — interface defined, not implemented this round.

## 7. H100 validation (devgpu014, 8×H100, driver 580.82.07)
- one soft failure → DEGRADED (NOT OFFLINE); sustained → OFFLINE at soft_failures=3 (~15.5s after inject).
- recovery: OFFLINE → RECOVERING (~2s after clear) → READY (~8s after clear, gated by hold+streak).
- readyz: single-device hard fault → 503 with reasons; healthy → 200.
- worker: WORKER_DISAPPEARED cause=unknown, mem_released=22GB; zero crash-confirmed.
- drain: POST changed:true / idempotent changed:false / GET 405.
- overhead (idle, 4 dev, access-probe each): **8.4% of one core, 44 MB RSS**, probe ~122ms, API p50 <0.5ms.

## 8. MI350X validation (devgpu499, 8×MI350X gfx950, ROCm 7.0)
- identical lifecycle correctness: one soft → DEGRADED; OFFLINE at soft_failures=3 (~19.5s — slower
  than H100; rocm-smi cadence ~220ms/probe × 6 devices + access probes).
- recovery via RECOVERING (~9s after clear).
- drain: POST changed:true / idempotent / GET 405 (after race fix).
- worker: per-card proc-count unreliable (stays 0 with live 25GB alloc) — AMD honest signal is
  memory-delta; no false crash claims. (Documented vendor gap, unchanged from round 1.)
- overhead (idle, 6 dev): **1.8% of one core, 18 MB RSS** (384-core host), probe ~220ms.

## 9. Mesh validation
- Collector sees all 10 backends (4 nvidia + 6 amd) over IPv6 with new schema (host_capacity_hint, stale flag).
- Unreachable backend (dead port) → `reachable:false` + error captured.
- Stale flag set when heartbeat age > 10s threshold (collector) / readiness TELEMETRY_STALE (sidecar, deterministic unit test).
- Disconnect/rejoin events recorded with recovery_duration_ms (round-1 mechanism intact + now carries evidence).

## 10. Remaining limitations
- **AMD worker attribution:** `rocm-smi --showpids` per-card mapping unreliable → AMD worker
  start/stop detection depends on memory-delta, not proc identity. No per-PID attribution on either vendor.
- **No confirmed crash/OOM cause:** requires supervised-process/cgroup/runtime integration (not built).
- **No runtime serving capacity:** plugin interface only; no vLLM/TGI adapter.
- **Security:** drain is method-guarded + validated + idempotent but UNauthenticated/UNencrypted;
  must bind localhost/trusted-mesh and front with mTLS+authz for production (see api_security_notes.md).
- **Overhead rose** with access-probe-each (H100 5.5%→8.4%); a cheaper liveness probe or longer
  access cadence would reduce it.
- **Detection latency is poll-bounded** and notably slower on AMD (≈4-5s effective cycle for 6 devices).

## 11. Next three steps (ranked)
1. **PID/cgroup worker attribution + confirmed termination cause** — supervise workers (or read
   cgroup/runtime events) so WORKER_EXIT_OBSERVED / WORKER_CRASH_CONFIRMED / WORKER_OOM_CONFIRMED can be
   emitted with ground truth, and fix the AMD proc-attribution gap.
2. **Cheaper/decoupled access probe** — separate a light liveness probe from the full telemetry sample
   and make access-probe cadence configurable, to cut the access-probe-each overhead (esp. AMD).
3. **Auth + transport security for mutations** — native mTLS + token authz + durable audit sink on
   `/v1/drain` so the mutation endpoint is safe beyond a trusted operator plane.
