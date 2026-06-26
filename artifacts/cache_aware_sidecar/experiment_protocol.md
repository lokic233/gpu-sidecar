# Experiment Protocol

Reproducible controlled-workload protocol for the cache-aware routing comparison (task §10).

## Stack under test (live E2E, 2026-06-25)

- **H100** (`devgpu014`): real vLLM 0.23.0 on `127.0.0.1:8000` (Qwen2.5-0.5B-Instruct).
- **MI350X** (`devgpu499`): `mini_oai_server.py` (HF transformers) on `127.0.0.1:8000`, same model.
- Cache-aware sidecars (explicit mode) on `:19097` (H100, MI350X) and `:19098` (second H100 logical
  backend for the equal-capability isolation run). Router on `127.0.0.1:19094`. Trajectory collector
  on `[::]:29110`.

Launch (H100):
```bash
./bin/trajcollector -listen "[::]:29110" -out artifacts/cache_aware_sidecar/e2e/h100/trajectories.jsonl &
./bin/sidecar -listen "[::]:19097" -devices 3 -data-plane -vllm-url http://127.0.0.1:8000 \
   -backend-id h100-gpu3 -dp-device 3 -collector-url http://127.0.0.1:29110/v1/events \
   -cache-observer explicit -cache-explicit-header-enabled -cache-stale-after 30s &
./bin/router -listen 127.0.0.1:19094 -policy cache_aware_estimated_finish -snapshot-interval 500ms \
   -collector-url http://127.0.0.1:29110/v1/events -backends '<H100+MI350X JSON>' &
```
MI350X sidecar identical with `-backend-id mi350x-gpu2 -devices 2` (under a restart watcher on the
contended box — `scripts/watch_mi350x_cache_sidecar.sh`).

## Workload dimensions

- **Prefix groups** (via opaque `X-Cache-Prefix-Key`, never content):
  - `hot`: 1 shared key, heavy reuse (default 50% of traffic).
  - `warm`: pool of 8 keys (30%).
  - `unique`: per-request key, no reuse (20%).
- **Request shapes** (`experiments/cache_harness.py:SHAPES`):
  - short-in/short-out (40c, 8tok), short-in/long-out (40c, 128tok),
    long-in/short-out (3200c, 8tok), long-in/long-out (3200c, 128tok).
- **Arrival modes**: `steady` (validated), `prefix_burst`, `phase_shift` (harness flags).

## Policies compared

`round_robin`, `least_queued`, `health_gated_least_pressure`, `cache_affinity_only`,
`cache_aware_estimated_finish`.

## Runners

- `experiments/cache_harness.py` — single-policy load generator (the router decides the actual
  policy; the harness only generates load + measures). Metrics: throughput, TTFT/E2E p50/p95,
  per-backend assignment, failures. No content logged.
- `experiments/cache_compare.py` — cross-vendor comparison: restarts the router under each policy,
  runs the harness, builds a table. Output: `artifacts/cache_aware_sidecar/e2e/comparison/`.
- `experiments/cache_compare_equal.py` — **equal-capability** isolation (two H100 backends → same
  fast vLLM; cache locality is the ONLY asymmetry). Measures HOT-prefix concentration on the warmed
  backend per policy. Output: `artifacts/cache_aware_sidecar/e2e/comparison_equal/`.

Run:
```bash
REQS=160 CONC=14 python3 experiments/cache_compare_equal.py
REQS=120 CONC=10 ARRIVAL=steady python3 experiments/cache_compare.py
```

## Metrics reported (task §10)
throughput, TTFT p50/p95, E2E p50/p95, queue wait, prefix-cache hit rate (aggregate from runtime),
per-backend assignment ratio, hot-prefix concentration, stale-cache fallback count
(`stale_invalidations_total` from `/v1/cache`), cache-hot overload incidents (HOT requests whose
chosen backend had `cache_confidence>=floor` AND queue near max — read from CANDIDATE_STATE).

## Why two comparison runs
The cross-vendor run uses REAL heterogeneous hardware (H100 vLLM ≫ MI350X HF), where latency-optimal
routing CORRECTLY concentrates on H100 — honest, but it masks the locality effect. The
equal-capability run isolates the locality variable so the policy-behavior thesis is visible without
the speed confound. Both are reported.

## Determinism / safety notes
- Explicit prefix keys make locality deterministic and runtime-independent (works on MI350X which
  has no real prefix cache).
- The MI350X box is heavily contended (load ~20-27) and externally SIGABRTs long-lived processes; a
  restart watcher keeps the sidecar up. This affects MI350X availability, NOT the correctness of the
  cache logic (identical binary is stable on H100).
