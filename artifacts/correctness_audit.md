# Correctness Audit (Round 2)

Each issue: previous behavior → correct behavior → code path → test → real-node evidence.

## 1. /readyz conflated readiness with GPU visibility
- **Previous:** `Supervisor.Ready()` returned true if ANY device had `GPUVisible==true` — even if
  the access probe was failing, telemetry was stale, or the device was OFFLINE.
- **Correct:** `Readiness()` requires per device: first collection done AND visible AND accessible
  AND last probe ok AND telemetry age < max AND not OFFLINE AND collector not stalled. Host-level
  `/readyz` is ready iff ≥1 device is ready and the collector isn't stalled; per-device detail in `details[]`.
- **Code:** `internal/engine/supervisor.go` (Readiness, collectorStalled), `internal/api/server.go` (readyz → 200/503 + structured).
- **Tests:** `supervisor_test.go` (NotReadyBeforeFirstCollection, NotReadyWhenInaccessible, NotReadyWhenStale, NotReadyWhenOffline, ReadyWhenHealthy); `server_test.go` (ReadyzHealthy, ReadyzInaccessible).
- **Real evidence:** single-device sidecar dev7: healthy → 200/ready; hard fault → **HTTP 503** with
  reasons `[GPU_NOT_VISIBLE, GPU_NOT_ACCESSIBLE, LAST_PROBE_FAILED, LIFECYCLE_OFFLINE]`
  (`validation_round_2/h100_raw/readyz_offline_single_device.json`).

## 2. OFFLINE had no soft-failure hysteresis
- **Previous:** `desired()` returned OFFLINE if `!GPUVisible || !GPUAccessible || failures>=N`, and
  `Step()` applied any OFFLINE target IMMEDIATELY. A single failed access probe forced OFFLINE.
- **Correct:** hard evidence (device gone / adapter-init fail / `!GPUVisible` without a soft
  classification) → OFFLINE immediately. Soft failures (timeout / one failed access probe /
  malformed output) → DEGRADED first; OFFLINE only after `OfflineFailures` consecutive soft
  failures. Recovery gated through RECOVERING by both `RecoveringHoldSec` AND `RecoveryStreak`.
- **Code:** `internal/core/lifecycle.go` (hard/soft split, reason codes), adapters classify failures
  (`nvidia.go` classifyNVFailure, `amd.go` classifyAMDFailure), `model.go` ProbeFailure.
- **Tests:** `lifecycle_test.go` (SoftFailureGoesDegradedNotOffline, SoftFailuresReachThresholdOffline,
  HardEvidenceImmediateOffline, RecoveryThroughRecovering, ConfigurableThreshold, NoOfflineReadyFlapping).
- **Real evidence H100:** one soft failure → DEGRADED (not OFFLINE); OFFLINE at exactly
  `soft_failures=3`; recovery passed through RECOVERING (`validate_round2_h100_*.json`).

## 3. Worker disappearance could read as a crash
- **Previous:** count decrease emitted `WORKER_STOP`; a `ProcessCrashes` stability input existed but
  was NEVER populated (dead input). Docs said "SIGKILL crash detected".
- **Correct:** evidence-only events. `WORKER_STARTED`/`WORKER_DISAPPEARED` carry an `evidence` map
  (prev/cur process count, memory_released_bytes) and `termination_cause="unknown"`,
  `ground_truth_source=""`. Confirmed-crash events exist as constants but are NOT emitted without
  direct evidence (supervised exit / cgroup / runtime). `ProcessCrashes` renamed
  `AbnormalDisappearances` (an observed, not confirmed, signal).
- **Code:** `internal/engine/supervisor.go` (detectWorkers), `internal/core/model.go` (event taxonomy),
  `internal/core/stability.go` (AbnormalDisappearances).
- **Tests:** `supervisor_test.go` (WorkerStartAndDisappearEvents asserts cause=unknown, no crash claim).
- **Real evidence H100:** `WORKER_DISAPPEARED | cause: unknown | gt_source: - | evidence:
  {memory_released_bytes: 22023241728, previous_process_count: 1, current_process_count: 0}`.

## 4. effective_capacity implied serving capacity
- **Previous:** `effective_capacity` (free_mem × (1-util) × stability), documented as "serving-capacity estimate".
- **Correct:** renamed `capacity.host_capacity_hint` with `capacity_semantics="heuristic_host_derived"`,
  explicit `components` map, and `runtime_serving_capacity_supported=false`. Added an OPTIONAL
  `RuntimeServingCapacity` plugin struct (queue depth, KV-cache, TTFT, …) that is nil for a pure host sidecar.
- **Code:** `internal/core/model.go` (CapacityHint, RuntimeServingCapacity), `supervisor.go` (computeCapacityHint),
  metrics renamed `gpu_host_capacity_hint`.
- **Tests:** `supervisor_test.go` + `server_test.go` assert semantics label and runtime-unsupported; metrics test asserts the old name is gone.
- **Real evidence:** `/v1/status` shows `capacity_semantics: heuristic_host_derived`, `runtime_serving_capacity_supported: false`, components exposed.

## 5. Drain mutated state via unauthenticated GET
- **Previous:** `GET /v1/drain?device=N&on=true` mutated state; `on` parsed with errors ignored.
- **Correct:** POST/PUT only (GET → 405); JSON or form body; both fields required & validated;
  idempotent (`changed` flag); records a lifecycle event with prev/new draining + request source.
  Bind address configurable; localhost/mesh-only guidance in api_security_notes.md.
- **Code:** `internal/api/server.go` (drain), `internal/engine/supervisor.go` (SetDraining returns found,changed + event).
- **Tests:** `server_test.go` (DrainRejectsGET=405, DrainPostValid, DrainPostMissingFields=400, DrainUnknownDevice=404, DrainIdempotent).
- **Real evidence:** POST drain → `{changed:true, draining:true}`; `GET /v1/drain` → **405**
  (`validate_round2_*.json` SIDECAR_drain_GET_rejected code=405).

## 6. Missing system-level tests
- **Added:** supervisor tests (mock adapter + injectable clock), API tests (httptest, incl. concurrent
  reads during polling), collector tests (httptest sidecars: healthy/unreachable/stale/malformed/slow/
  mixed-vendor/duplicate-id). Total tests 32 → 76. `go test -race ./...` passes clean.
