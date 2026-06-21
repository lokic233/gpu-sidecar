# Readiness Semantics

Three distinct concepts that the round-1 prototype conflated, now separated:

| Endpoint / signal | Question it answers |
|---|---|
| `/healthz` | Is the sidecar **process** alive and serving HTTP? (always 200 if it can respond) |
| `/readyz` | Can the sidecar **currently provide trustworthy, fresh inspection** of its GPU backend? |
| `lifecycle_state` | Should the backend **receive traffic**? (READY/BUSY ok; DEGRADED/DRAINING/OFFLINE/RECOVERING are routing decisions) |

These MUST NOT be conflated. `/readyz` is about the *sidecar's ability to inspect*, not about whether
the GPU should serve work.

## /readyz contract (per device)
A device is READY-for-inspection iff ALL hold:
1. At least one collection cycle completed (`collected`).
2. `gpu_visible == true`.
3. `gpu_accessible == true` (active access probe succeeded).
4. The latest required probe succeeded (`lastProbeOK`).
5. Telemetry age `< max_telemetry_age` (configurable, default 15s).
6. `lifecycle_state != OFFLINE`.
7. The sidecar collector is not stalled (newest sample younger than 3× poll interval).

Host-level `/readyz` returns **200** iff ≥1 device is ready AND the collector is not stalled;
otherwise **503**. The response body always includes per-device `details[]` with `ready` + `reasons[]`.

## Disposition of non-READY lifecycle states for /readyz
| State | /readyz device-ready? | Rationale |
|---|---|---|
| READY | yes | healthy, inspectable |
| BUSY | yes | inspectable; capacity-constrained is a routing concern, not an inspection failure |
| DEGRADED | **yes (if probe currently OK + fresh)** | the sidecar can still truthfully report a degraded backend; the *router* decides whether to avoid it |
| DRAINING | yes | inspectable; operator chose to drain — routing concern |
| RECOVERING | yes | inspectable and probes are succeeding; the router may treat it cautiously |
| OFFLINE | **no** | the sidecar cannot meaningfully inspect the backend |

Note: a DEGRADED device whose *current* probe is failing (e.g. mid soft-failure streak) will fail
readiness via criteria 2–4 even though its lifecycle is DEGRADED. Readiness is about the latest
probe + freshness; lifecycle carries the smoothed history.

## Real-hardware evidence
- Single-device sidecar (H100 dev7): healthy → `200 {ready:true}`; hard fault →
  **`503 {ready:false, reasons:[GPU_NOT_VISIBLE, GPU_NOT_ACCESSIBLE, LAST_PROBE_FAILED, LIFECYCLE_OFFLINE]}`**
  (`validation_round_2/h100_raw/readyz_offline_single_device.json`).
- Multi-device host: when only 1 of 4 GPUs went OFFLINE, host `/readyz` stayed 200 (3 healthy GPUs),
  but that device's `details[].ready` was false with reasons — correct multi-device behavior.
- Stale telemetry: deterministic unit test `TestSupervisor_NotReadyWhenStale` (advance clock past
  max age without polling → `TELEMETRY_STALE`/`COLLECTOR_STALLED`).

---

## Round-3 update — multi-GPU aggregate + per-device readiness

Host `/readyz` now returns explicit aggregate fields so partial multi-GPU failure is unambiguous:
```json
{
  "control_plane_ready": true,
  "any_device_ready": true,
  "all_devices_ready": false,
  "ready_device_count": 7,
  "total_device_count": 8,
  "details": [ { "device_id": "3", "ready": false, "reasons": ["GPU_NOT_ACCESSIBLE"] }, ... ]
}
```
- **`control_plane_ready`** (== legacy `ready`): the sidecar collected, is not stalled, and can provide
  trustworthy status for **at least one** managed device. This is sidecar control-plane readiness — it
  is **NOT** proof that every GPU can receive traffic.
- **`any_device_ready` / `all_devices_ready`**: make partial readiness explicit.
- **`ready_device_count` / `total_device_count`**: counts for a router to reason about.
- Backward-compat: `ready`, `ready_devices`, `total_devices` are retained as aliases.

### Per-device readiness — `GET /readyz?device=N`
- **200** when device N satisfies the readiness contract.
- **503** when device N does not (with structured `reasons[]`).
- **404** when N is not a managed device.
Use this to decide traffic for a *specific* GPU; use host `/readyz` for sidecar control-plane health.
