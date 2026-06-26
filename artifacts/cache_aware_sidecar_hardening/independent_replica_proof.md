# Independent Replica Proof (P0 #2)

Evidence that the Round-5 equal-capability experiment uses TWO GENUINELY INDEPENDENT vLLM replicas —
not two sidecars over one runtime (the invalid setup the prior round used). Captured live on
`devgpu014` (8× H100), 2026-06-26.

## Two independent vLLM processes, two GPUs

| | replica A | replica B |
|---|---|---|
| GPU id | **6** | **7** |
| GPU mem used | 30093 MiB (model loaded) | 30089 MiB (model loaded) |
| vLLM port | 8006 | 8007 |
| vLLM PID | 2501868 | 2501869 |
| ZMQ KV-event port | 5560 | 5561 |
| vLLM version | 0.23.0 | 0.23.0 |
| model | Qwen/Qwen2.5-0.5B-Instruct | Qwen/Qwen2.5-0.5B-Instruct |
| dtype / TP / max-len / gpu-util / flags | bf16 / 1 / 4096 / 0.30 / --enforce-eager | identical |
| sidecar port | 19101 | 19102 |
| sidecar PID | 3338632 | 3338633 |
| sidecar backend-id | replicaA | replicaB |
| `runtime_endpoint_id` | `08d83fb2e8855eea` | `66326ea9a3b13db8` (DISTINCT) |

Launched via `artifacts/cache_aware_sidecar_hardening/launch_two_replicas.sh`
(`CUDA_VISIBLE_DEVICES=6` and `=7`, distinct ports, distinct ZMQ endpoints).

## Separate /metrics endpoints
- `http://127.0.0.1:8006/metrics` and `http://127.0.0.1:8007/metrics` are distinct processes.
- `http://127.0.0.1:8006/version` and `:8007/version` both report vLLM 0.23.0.

## Separate KV caches / schedulers — TRAFFIC ISOLATION PROOF
With both replicas idle, 5 requests were sent to **replica A only** (port 8006):

```
A vllm:prefix_cache_queries_total:  36.0 -> 221.0   (changed: A served the requests)
B vllm:prefix_cache_queries_total:   0.0 ->   0.0   (UNCHANGED: B was not touched)
```

Traffic to A did not move B's prefix-cache counters at all → the two replicas have **independent KV
caches, independent prefix caches, independent schedulers, and independent continuous batches.** This
is the property the prior shared-runtime experiment lacked.

## Runtime-identity guard (refuses the invalid setup)
`experiments/cache_compare_equal.py` now calls `assert_independent_replicas()` BEFORE every policy
run. It reads each backend's `runtime_endpoint_id` (robust: hash of host+vllm-url+boot; surfaced by
the sidecar in `/v1/runtime`, materialized into `BackendState`) and `runtime_instance_id`
(process_start_time_seconds, secondary). If two backends share an identity → **HARD STOP** with a
clear message; the experiment refuses to run. (Verified: two sidecars over one vLLM share an
endpoint id and are rejected; the two real replicas above have distinct ids and are accepted.)

> Note on `runtime_instance_id`: vLLM's `process_start_time_seconds` is exported at ~12 sig-fig
> precision, so two replicas launched within the same ~10ms can collide on it (observed: both
> 1782463752.6). That is exactly why `runtime_endpoint_id` (host+url+boot hash) is the PRIMARY signal
> — it is distinct per vLLM endpoint regardless of start time.
