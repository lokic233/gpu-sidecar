# Normalized Cross-Vendor Schema

> **Superseded in part by Round 2 (correctness).** See `final_hardening_report.md`, `readiness_semantics.md`, `lifecycle_hysteresis.md`, `worker_event_semantics.md`, `capacity_semantics.md`. Notably: `effective_capacity`→`host_capacity_hint` (heuristic); OFFLINE now uses hard/soft hysteresis; lifecycle exposes `reason_codes`.

The sidecar exposes one JSON contract for both NVIDIA and AMD. Every metric is wrapped in a
`Field{value, supported}` so consumers can distinguish "real value" from "not available on this
vendor/tool". Source: `internal/core/model.go`.

## Identity (stable, per device)
| Field | Meaning | NVIDIA source | AMD source |
|---|---|---|---|
| sidecar_instance_id | unique sidecar run id | hostname+timestamp | same |
| hostname | host fqdn | `os.Hostname` | same |
| backend_id | routing key `host-gpuN` | derived | derived |
| vendor | `nvidia`/`amd`/`unknown` | adapter | adapter |
| device_id | vendor index | nvidia-smi index | rocm-smi cardN |
| gpu_model | model string | nvidia-smi name | rocm-smi Card Series |
| gpu_uuid | stable hw id | nvidia-smi UUID | rocm-smi Unique ID (fallback Serial) |
| driver_version | driver | nvidia-smi | rocm-smi --showdriverversion |
| runtime_version | CUDA/ROCm | nvidia-smi banner | rocm-smi --version |
| boot_id | host boot id | /proc/sys/kernel/random/boot_id | same |
| sidecar_version | build version | const | const |

## Instantaneous Health (per device, per cycle)
| Field | Type | NVIDIA | AMD | Notes |
|---|---|---|---|---|
| timestamp | time | ✅ | ✅ | wall clock for humans |
| heartbeat_ok | bool | ✅ | ✅ | sidecar reached this code path |
| gpu_visible | bool | ✅ | ✅ | sample returned parseable data |
| gpu_accessible | bool | ✅ | ✅ | active access probe result |
| utilization_gpu_pct | Field[f64] | ✅ util.gpu | ✅ GPU use (%) | |
| mem_used_bytes | Field[u64] | ✅ | ✅ VRAM Total Used | NVIDIA MiB→bytes |
| mem_free_bytes | Field[u64] | ✅ | ✅ derived (total-used) | |
| mem_total_bytes | Field[u64] | ✅ | ✅ VRAM Total | |
| temperature_c | Field[f64] | ✅ temperature.gpu | ✅ junction temp | |
| power_watts | Field[f64] | ✅ power.draw | ✅ Socket Graphics Power | |
| power_limit_watts | Field[f64] | ✅ power.limit | ❌ **not queried by rocm-smi here** | marked unsupported on AMD |
| sm_clock_mhz | Field[f64] | ✅ clocks.sm | ✅ sclk | |
| mem_clock_mhz | Field[f64] | ✅ clocks.mem | ✅ mclk | |
| compute_proc_count | Field[int] | ✅ query-compute-apps | ✅ --showpids | |
| effective_free_mem_ratio | f64 | ✅ derived | ✅ derived | mem_free/mem_total |
| probe_latency_ms | f64 | ✅ measured | ✅ measured | telemetry collection time |
| ecc_uncorrectable_total | Field[u64] | ✅ ecc.errors.uncorrected.aggregate | ❌ AMD uses RAS | |
| ecc_correctable_total | Field[u64] | ✅ | ❌ | |
| nvidia_xid_errors | Field[[]int] | ✅ dmesg scrape | ❌ vendor-specific | |
| amd_ras_uncorrectable | Field[u64] | ❌ | ✅ --showrasinfo | |
| amd_ras_correctable | Field[u64] | ❌ | ✅ --showrasinfo | |
| unsupported_fields | []string | ✅ | ✅ | human-readable list of what was N/A this cycle |

## Lifecycle / Reliability / Stability
- `lifecycle_state`: one of UNKNOWN/READY/BUSY/DEGRADED/DRAINING/OFFLINE/RECOVERING (see lifecycle_state_machine.md)
- `reliability`: last successful/failed probe, consecutive failures, sidecar/worker start counts,
  disconnect/rejoin counts, recovery duration, recent availability/failure rate, latency p50/p95, throughput variance.
- `stability`: `{score [0,1], components{...}, updated_at}` (see stability_score.md). Components are exposed for auditing.

## Routing-facing fields (collector BackendView)
| Field | Meaning |
|---|---|
| lifecycle_state | current state — router should avoid OFFLINE/DRAINING, treat DEGRADED with caution |
| stability_score | [0,1] confidence the backend is reliable now AND recently; carries memory of instability |
| effective_capacity | [0,1] = free_mem_ratio × (1−util) × stability — estimated headroom to accept work |
| last_heartbeat_age_ms | freshness of the data; large age ⇒ stale/suspect |
| recent_failure_count | consecutive probe failures |
| recovering | true while a recently-offline backend is in its RECOVERING hold |
| reachable | collector could fetch /v1/status at all |

The collector is explicitly NOT a scheduler. It normalizes; routing decisions live in a separate control plane.
