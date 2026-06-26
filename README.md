# GPU Host Sidecar (cross-vendor: NVIDIA H100 + AMD MI350X)

> **Round 5 — cache-aware admission sidecar.** The sidecar now also exposes **cache-locality** state
> so a global router can route on *prefix reuse + capacity*, not just load. Added a pluggable
> cache-observation plane (`internal/cache`: bounded thread-safe prefix index +
> `disabled|explicit|vllm_events` providers), a cache-aware analytical routing baseline
> (`cache_aware_estimated_finish`, documented coefficients, no magic constants), `GET /v1/cache` +
> bounded `gpu_cache_*` metrics, optional token-level work accounting, and full per-candidate RL state
> emission. **Validated E2E on real vLLM on BOTH H100 (vLLM 0.23) and MI350X (vLLM 0.21.1, ROCm 7.0).**
> Native vLLM KV events were captured live on **both** vendors with an **identical schema**; per-request
> native block matching remains the one documented blocker (it needs raw token IDs + vLLM-internal
> hashing), so `vllm_events` is metadata-only and the **validated routing path is the deterministic
> explicit-prefix provider + safe load-only fallback.** All cache features default **OFF**. 178 tests,
> race-clean. See `README.md` §"Cache-aware quickstart" + `artifacts/cache_aware_sidecar/`
> (`implementation_report.md`, `vllm_cache_observability_audit.md`).

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

The sidecar has **two planes** that never block each other: a slow OBSERVATION plane (GPU telemetry,
lifecycle, runtime metrics, cache locality) and a hot DATA plane (admit → bounded queue → relay to
vLLM). A separate global router reads an already-materialized snapshot (no per-request scraping).

```diagram
 Client (OpenAI-compatible HTTP, stream=true|false)
   │  POST /v1/chat/completions
   ▼
 Global Router Gateway                    cmd/router · internal/router
   │  reads MATERIALIZED snapshot (polled off the hot path; NO per-request scrape)
   │  policy.SelectBackend(features, snapshot) → backend            ← cache-aware here
   │  bounded pre-first-token retry · cancellation · async trajectory
   ▼
 ┌──────────────────── one sidecar per GPU host (cmd/sidecar) ─────────────────────┐
 │  OBSERVATION plane (slow loops, never on hot path):                              │
 │   vendor adapter (nvidia-smi+dmesg | rocm-smi+RAS | generic) → engine.Supervisor │
 │     → lifecycle FSM (hysteresis) · stability score · bounded history             │
 │   vLLM runtime adapter (/metrics: running/waiting/kv_util/prefix_cache/…)        │
 │   CACHE-observation plane (internal/cache, default OFF):                         │
 │     provider = disabled | explicit | vllm_events                                 │
 │     bounded prefix INDEX (metadata only — never raw tokens) + KV-headroom feed   │
 │  DATA plane (hot): admit → bounded FIFO queue → dispatch → own vLLM conn → relay │
 │     + explicit-prefix header extract/HASH/STRIP · optional token work accounting │
 │  HTTP: /healthz /readyz /v1/status /v1/history /v1/events /v1/drain /metrics     │
 │        /v1/runtime  /v1/queue  /v1/cache  /v1/chat/completions                   │
 └──────────────────────────────────────────────────────────┬──────────────────────┘
                                                             │ local vLLM (H100 0.23 / MI350X 0.21 ROCm)
   Router + sidecars ──async, bounded, batched──► Response/Trajectory Collector (cmd/trajcollector)
   collector (cmd/collector) ──► one normalized BackendView table across all hosts (NOT a scheduler)
```

## Layout
```
cmd/sidecar       daemon (per host): observation plane + optional -data-plane (proxy/queue/cache)
cmd/router        Global Router Gateway: materialized snapshot + policy + relay + retry + trajectory
cmd/collector     mesh aggregator -> normalized backend table / JSON
cmd/trajcollector async Response/Trajectory Collector (JSONL; NOT a proxy)
internal/core     model, lifecycle state machine, stability score, bounded history
internal/adapters nvidia, amd, generic adapters behind one interface + detect
internal/exec     defensive subprocess runner (timeout/bounded/exit-code)
internal/engine   supervisor (probe loop, worker/disconnect detection)
internal/runtime  vLLM runtime adapter (/metrics parser; running/waiting/kv_util/prefix_cache/…)
internal/dataplane bounded admission queue + OpenAI proxy + SSE relay + token work accounting
internal/cache    pluggable cache-observation plane: prefix index + disabled/explicit/vllm_events
internal/router   registry (snapshot+cache directory) · policies (incl. cache_aware) · gateway
internal/trajectory non-blocking bounded batched event emitter
internal/api      HTTP endpoints + Prometheus metrics
experiments/      CUDA/HIP workloads + detect/crash/overhead + cache harness/compare + kv_event_bridge
scripts/          one-command test / launch / backend-table
artifacts/        environment inventory, schemas, results, raw logs, reports (incl. cache_aware_sidecar/)
```

## Install / build (per node — needs Go 1.26+, no root)
```bash
git clone https://github.com/lokic233/gpu-sidecar && cd gpu-sidecar
go build -o bin/sidecar      ./cmd/sidecar       # per-host daemon (+ optional -data-plane)
go build -o bin/router       ./cmd/router        # global router gateway (cache-aware policy)
go build -o bin/collector    ./cmd/collector     # mesh aggregator -> normalized table
go build -o bin/trajcollector ./cmd/trajcollector # async trajectory collector (optional)
# or just: go build ./...
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

## Cache-aware quickstart (router + data-plane sidecar + cache observation)

This brings up the full Round-5 path: one (or more) GPU host(s) each run a `-data-plane` sidecar with
cache observation on; a single router fronts them with the cache-aware policy. Everything is
**OpenAI-compatible**, so you talk to the router exactly like a vLLM server. **Cache features default
OFF** — you opt in with `--cache-observer`.

**1. Per GPU host — vLLM + a data-plane sidecar with cache observation.**
```bash
# local vLLM (any backend the runtime adapter can scrape /metrics from)
vllm serve Qwen/Qwen2.5-0.5B-Instruct --port 8000        # H100: vLLM 0.23
# (AMD: real ROCm vLLM works too — see artifacts/cache_aware_sidecar/e2e/mi350x_realvllm/launch_real.sh)

./bin/sidecar --listen "[::]:19097" --devices 3 \
  --data-plane --vllm-url http://127.0.0.1:8000 --backend-id h100-gpu3 --dp-device 3 \
  --max-queued 256 --max-inflight 64 --queue-timeout 30s \
  --collector-url http://127.0.0.1:29110/v1/events \
  --cache-observer explicit --cache-explicit-header-enabled --cache-stale-after 30s
```

**2. One router fronting the backends with the cache-aware policy.**
```bash
./bin/trajcollector --listen "[::]:29110" --out traj.jsonl &     # optional (async; never blocks)
./bin/router --listen 127.0.0.1:19094 --policy cache_aware_estimated_finish \
  --snapshot-interval 500ms --collector-url http://127.0.0.1:29110/v1/events --max-retries 1 \
  --backends '[{"id":"h100-gpu3","vendor":"nvidia","sidecar_url":"http://127.0.0.1:19097","snapshot_url":"http://127.0.0.1:19097"},
               {"id":"mi350x-gpu2","vendor":"amd","sidecar_url":"http://[<mesh-ipv6>]:19097","snapshot_url":"http://[<mesh-ipv6>]:19097"}]'
```

**3. Drive it like vLLM.** For deterministic prefix-group experiments, attach the opaque experiment
headers (hashed by the sidecar, **stripped before vLLM**, never logged):
```bash
curl -s http://127.0.0.1:19094/v1/chat/completions -H 'Content-Type: application/json' \
  -H 'X-Cache-Prefix-Key: hotgroup-A' -H 'X-Cache-Prefix-Tokens: 512' \
  -d '{"model":"Qwen/Qwen2.5-0.5B-Instruct","messages":[{"role":"user","content":"hi"}],"max_tokens":8}'

curl -s http://127.0.0.1:19094/v1/backends         # what the router currently sees (materialized)
curl -s http://127.0.0.1:19097/v1/cache            # one sidecar's cache-locality metadata
```

**Cache-observer modes** (`--cache-observer`):
| mode | when to use |
|---|---|
| `disabled` (default) | no cache observation; router reduces to the load-only estimate |
| `explicit` | deterministic experiments via `X-Cache-Prefix-Key`; **cross-vendor, validated** |
| `vllm-events` | ingest native vLLM KV events (metadata-only; per-request match is unsupported — see audit) |

**Policies** (`--policy`): `round_robin`, `least_queued`, `least_runtime_waiting`,
`health_gated_least_pressure`, `cache_affinity_only`, `cache_aware_estimated_finish`.

**Reproducible comparison harness:**
```bash
python3 experiments/cache_harness.py --router http://127.0.0.1:19094 --requests 160 --concurrency 12
REQS=160 CONC=14 python3 experiments/cache_compare_equal.py     # isolates locality (equal-speed backends)
```

## What the router retrieves for routing

The router **never scrapes vLLM/GPU telemetry on the request hot path.** A background loop
(`--snapshot-interval`, default 500ms) polls each backend's sidecar — `/readyz`, `/v1/status`,
`/v1/queue`, `/v1/runtime`, `/v1/cache` — and materializes an immutable in-memory **snapshot** + a
bounded **cache directory**. The policy reads that snapshot (pure, no I/O) and does an **O(1) local
map lookup** for the current request's prefix key. Per-backend materialized state
(`internal/router.BackendState`, also visible at `GET /v1/backends`):

| group | fields | source |
|---|---|---|
| **reachability/health** | `reachable`, `runtime_healthy`, `lifecycle_state`, `control_plane_ready`, `stability_score`, `host_capacity_hint`, `snapshot_age_ms` | `/readyz`, `/v1/status` |
| **admission queue** (host) | `queue_depth`, `queue_inflight`, `queue_max` | `/v1/queue` |
| **vLLM runtime** (distinct queue) | `runtime_waiting`, `runtime_running`, `kv_cache_util` | `/v1/runtime` |
| **service rate** | `gen_tokens_per_sec`, `service_rate_supported` — a **delta** of the cumulative `generation_tokens_total` counter over wall time (never the raw total) | `/v1/runtime` (differenced) |
| **cache locality** | `cache_observation_supported`, `cache_match_supported`, `cache_ready`, `cache_confidence`, `cache_snapshot_age_ms`, `cache_event_sequence`, `cache_reset_epoch`, `cache_index_size`, `cache_provider`, `kv_headroom`, `kv_headroom_supported` | `/v1/cache` |
| **per-request match** | matched prefix tokens for *this* request's `X-Cache-Prefix-Key` | router-local cache directory (O(1)) |

**The cache-aware policy** (`cache_aware_estimated_finish`) turns that into a transparent finish-time
estimate (lower wins; ties broken by backend id so the chosen logical backend is order-independent):
```
estimated_finish_ms = est_queue_ms
                    + est_prefill_ms(uncached_prompt_tokens)   # uncached = input − matched_prefix
                    + est_decode_ms(expected_output)           # uses measured service rate when reliable
                    + cache_staleness_penalty_ms               # ×(1−confidence) when plane supported
                    + kv_pressure_penalty_ms                   # ×(1−kv_headroom) when measurable
```
Coefficients are explicit/documented (`DefaultCacheAwareConfig`, no magic constants), e.g.
`PrefillMsPerToken=0.05`, `QueueMsPerQueued=8.0`, `KVPressurePenaltyMs=40.0`, `ConfidenceFloor=0.30`.
Safety: if cache observation is **unsupported / stale / below the confidence floor**, locality is
ignored and the policy falls back to the load-only estimate — a cache-hot but **overloaded** backend
can still lose. Per-decision the router emits a `CANDIDATE_STATE` trajectory event with the full
breakdown (`uncached_prompt_tokens`, `matched_prefix_tokens`, `match_ratio`, `est_*_ms`,
`estimated_prefill_saved_ms`, `final_analytical_score_ms`, …) — the **base score** over which an RL
policy can later learn a residual (see `artifacts/cache_aware_sidecar/cache_aware_rl_state_contract.md`).

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
| `/metrics` | Prometheus exposition (incl. bounded-cardinality `gpu_cache_*` when cache is enabled) |
| `/v1/runtime` | materialized vLLM runtime snapshot (running/waiting, `kv_cache_utilization`, `generation_tokens_total`, `prefix_cache_*`; each `{value,supported}`). Only when `-data-plane`. |
| `/v1/queue` | host admission-queue metrics (depth/inflight/rates/wait p50,p95) + optional `work_accounting` (reserved/active prefill+decode tokens). Only when `-data-plane`. |
| `/v1/cache` | **cache-locality metadata** (bounded): `enabled,provider,supported,match_supported,ready,confidence,snapshot_age_ms,last_event_sequence,cache_reset_epoch,index_entries,kv_headroom,…` + an opaque hashed `directory`. Reports `enabled:false` unless `--cache-observer` is set. |
| `POST /v1/chat/completions` | OpenAI-compatible proxy (admit→queue→relay, streaming + non-stream). Only when `-data-plane`. |

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
