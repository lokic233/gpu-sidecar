# GPU Host Sidecar (cross-vendor: NVIDIA H100 + AMD MI350X)

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

## Endpoints
| Endpoint | Purpose |
|---|---|
| `/healthz` | sidecar **process** alive (always 200 if serving) |
| `/readyz` | sidecar can currently **inspect** its GPU backend (200/503) |
| `/v1/status` | full normalized host+device state |
| `/v1/history` | bounded recent time-series (`?device=N`) |
| `/v1/events` | bounded transition/failure events |
| `/v1/drain?device=N&on=true` | operator graceful-drain toggle |
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
