# GPU Host Sidecar (cross-vendor: NVIDIA H100 + AMD MI350X)

> **Round 4 — end-to-end vLLM flow.** Added a Global Router Gateway (`cmd/router`), a local data
> plane in the sidecar (`-data-plane`: bounded admission queue + OpenAI proxy + transparent SSE relay
> via `internal/dataplane`), a vLLM runtime adapter (`internal/runtime/vllm`), and an async
> Response/Trajectory Collector (`cmd/trajcollector`). Validated end-to-end on real H100 (vLLM 0.23)
> and real MI350X (gfx950): Client→Router→Sidecar queue→runtime→relay→Client, streaming + non-stream,
> cross-vendor round-robin, queue-full/drain/retry/cancel/collector-outage, joined trajectories.
> Proxy overhead ~0ms added TTFT. See `artifacts/e2e_vllm_flow/final_e2e_report.md`. 130 tests, race-clean.


> **Round 3 — final correctness/semantics polish.** Recovery is now **latched** (no DEGRADED/BUSY
> bypass of the hold+streak after OFFLINE); worker disappearance is **neutral by default** (only
> confirmed-abnormal/OOM/rapid-restart evidence lowers stability); worker-event history is **bounded**
> (age + size); default bind is **loopback-only**; host `/readyz` exposes **multi-GPU aggregate
> fields** + a **per-device `/readyz?device=N`**. 112 tests, race-clean. Authoritative report:
> `artifacts/final_polish/final_polish_report.md`.
>
> **Round 2 — correctness-hardened.** Readiness requires fresh+accessible telemetry (not just GPU
> visibility); OFFLINE uses hard/soft-failure hysteresis; worker disappearance reported with
> `termination_cause=unknown` (never a confirmed crash from count deltas); capacity is an explicit
> `host_capacity_hint` (heuristic), not serving capacity; drain is POST-only/validated/idempotent.
> See `artifacts/final_hardening_report.md` and `artifacts/correctness_audit.md`.

A lightweight, user-space, single-binary sidecar that continuously and **truthfully** reports
the current and historical condition of a GPU backend so a separate control plane (router /
scheduler) can make routing and capacity decisions. It is **host-level infrastructure** — not an
agent-runtime optimizer, LLM router, or scheduler.

It answers one question: *can a lightweight user-space sidecar provide accurate, timely signals
to distinguish healthy / overloaded / unstable / disconnected / recovering GPU backends across
both NVIDIA and AMD?* Validated live on a real H100 node and a real MI350X node — see
`artifacts/final_report.md`.

## Architecture
```diagram
   ┌─────────────────────────── one sidecar per GPU host ───────────────────────────┐
   │                                                                                 │
   │  vendor adapter (nvidia: nvidia-smi+dmesg | amd: rocm-smi+RAS | generic)        │
   │        │  defensive exec: hard timeout, bounded output, captured exit          │
   │        ▼                                                                        │
   │  engine.Supervisor  ── per-device probe loop ──►  history (bounded rings)       │
   │        │                                          reliability accounting        │
   │        ├──► lifecycle state machine (hysteresis, monotonic time)                │
   │        ├──► stability score (asymmetric EWMA, audited components)               │
   │        ▼                                                                        │
   │  HTTP API: /healthz /readyz /v1/status /v1/history /v1/events /v1/drain /metrics│
   └─────────────────────────────────────────────────────────────────────────────┬─┘
                                                                                   │ poll over mesh (IPv6)
   ┌───────────────────────────────────────────────────────────────────────────┐ │
   │  collector  ──►  one normalized BackendView table across all hosts          │◄┘
   │  (NOT a scheduler — normalizes only)                                        │
   └───────────────────────────────────────────────────────────────────────────┘
```

## Layout
```
cmd/sidecar      daemon (per host)
cmd/collector    mesh aggregator -> normalized backend table / JSON
internal/core    model, lifecycle state machine, stability score, bounded history
internal/adapters nvidia, amd, generic adapters behind one interface + detect
internal/exec    defensive subprocess runner (timeout/bounded/exit-code)
internal/engine  supervisor (probe loop, worker/disconnect detection)
internal/api     HTTP endpoints + Prometheus metrics
experiments/     CUDA/HIP workloads + detection/crash/disconnect/overhead harnesses
scripts/         one-command test / launch / backend-table
artifacts/       environment inventory, schemas, results, raw logs, final report
```

## Install / build (per node — needs Go 1.21+, no root)
```bash
git clone https://github.com/lokic233/gpu-sidecar && cd gpu-sidecar
go build -o bin/sidecar ./cmd/sidecar
go build -o bin/collector ./cmd/collector
```

## One-command paths
```bash
scripts/run_tests.sh                       # local test path (vet + unit tests)
scripts/launch_sidecar.sh 19095 3,4,6,7    # launch on a node (auto-detects vendor)
scripts/backend_table.sh <h100_url> <mi350x_url>   # normalized table across both
```

> **Bind address:** the sidecar defaults to **`127.0.0.1:9095` (loopback-only)** because the API
> includes an unauthenticated mutation endpoint (`/v1/drain`). To expose it on a trusted mesh,
> pass `--listen` explicitly (e.g. `--listen [::]:19095`); a WARNING is logged for non-loopback binds.
> See `artifacts/api_security_notes.md`. No production security is claimed.

## Endpoints
| Endpoint | Purpose |
|---|---|
| `/healthz` | sidecar **process** alive (always 200 if serving) |
| `/readyz` | **host control-plane** readiness: 200 if collected, not stalled, and ≥1 managed device is trustworthy/fresh/not-OFFLINE. Exposes `control_plane_ready`, `any_device_ready`, `all_devices_ready`, `ready_device_count`, `total_device_count` + per-device `details[]`. NOT proof every GPU can serve. See `artifacts/readiness_semantics.md` |
| `/readyz?device=N` | **per-device** readiness: 200 ready / 503 not-ready / 404 unmanaged, with structured reason codes |
| `/v1/status` | full normalized host+device state |
| `/v1/history` | bounded recent time-series (`?device=N`) |
| `/v1/events` | bounded transition/failure events |
| `POST /v1/drain` | operator graceful-drain toggle (**POST/PUT only**, GET→405); JSON `{"device":"N","on":true}`; validated, idempotent, audited. See `artifacts/api_security_notes.md` |
| `/metrics` | Prometheus exposition |

The status response distinguishes: sidecar-alive-but-GPU-inaccessible, GPU-visible-but-unhealthy,
GPU-healthy-but-capacity-constrained, backend-recovering, and unsupported vendor metrics.

## Vendor support & honesty
Metrics are wrapped in `Field{value, supported}`. NVIDIA-only fields (XID, ECC) are marked
unsupported on AMD; AMD-only fields (RAS) are marked unsupported on NVIDIA; `power_limit` is marked
unsupported on AMD because `rocm-smi` did not expose it in the queried set and **`amd-smi` is
permission-blocked** on the test host (user not in `render`/`video` groups). Nothing is fabricated.

## Limitations
See `artifacts/final_report.md` §9. Key ones: worker lifecycle is inferred from compute-process
*count deltas* (not per-PID cgroup attribution); XID/RAS scraping depends on readable `dmesg`;
`amd-smi` richer metrics unavailable without group membership; no privileged RAS/ECC injection
(prohibited). Detection latency is bounded below by the poll interval.
