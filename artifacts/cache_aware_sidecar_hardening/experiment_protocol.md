# Experiment Protocol — Round-5 Hardening

All runs labeled with date, git commit, GPU, runtime/version, model, config, #replicas, replica
independence, cache observer mode, and whether locality is synthetic or native.

## Common
- git commit: f642770 (branch navi/cache-hardening). Model Qwen/Qwen2.5-0.5B-Instruct.
- Cache observer: explicit (deterministic, cross-vendor). Locality: SYNTHETIC (explicit-prefix) but
  each prefix key is derived from a REAL identical prompt prefix (same key <-> same shared prompt
  text), so the synthetic grouping corresponds to real shared content. Real prefix-cache reuse is
  independently confirmed via each replica's vllm:prefix_cache_hits_total delta.

## A. Two INDEPENDENT equal-capability replicas (H100)
- replicaA: vLLM 0.23.0 on GPU6 :8006, sidecar :19101. replicaB: vLLM 0.23.0 on GPU7 :8007, sidecar
  :19102. Distinct runtime_endpoint_ids (see independent_replica_proof.md). Identical config.
- Driver: `experiments/cache_compare_equal.py` (HARD-STOP guard active). Workload 60% hot / 20% warm
  / 20% unique. Concurrency sweep 1,4,8,16,32. Policies: round_robin, least_queued,
  health_gated_least_pressure, cache_affinity_only, cache_aware_estimated_finish.
- Run: `REQS=120 CONCS=1,4,8,16,32 python3 experiments/cache_compare_equal.py`
- Output: results_equal/comparison.json + run.log (guard PASSED lines).

## B. Phase-shift (H100, two independent replicas)
- Phases: unique_heavy -> hot_burst -> warm_groups -> unique_heavy. Driver:
  `experiments/cache_phase_shift.py` (N=120, conc=12). Records per-phase per-backend assignment,
  TTFT p95, and REAL prefix-cache hit delta per replica.
- Output: results_equal/phase_shift.json.

## C. Heterogeneous H100 + MI350X (REAL vLLM on BOTH)
- h100: vLLM 0.23.0 GPU6 :8006 (profile decode 0.3 ms/tok). mi350x: real ROCm vLLM 0.21.1 :8001
  (profile decode 1.5 ms/tok). Router :19096 with `--profiles`. NOT a real-vs-mini-HF comparison.
- Purpose: verify heterogeneous profiles respected; strong-hardware concentration allowed when
  optimal; NO utilization-fairness reward.
- Output: results_hetero/hetero.json.

## Metrics captured
throughput, TTFT/E2E p50/p95/p99, per-backend + per-prefix assignment, real prefix-cache hit delta,
guard pass/fail. (KV util, preemptions, WARMING duration, false-ready, gap/stale fallback are
available via /v1/cache + /v1/queue + CANDIDATE_STATE for deeper analysis.)

## Honesty constraints honored
- No two-sidecars-over-one-runtime result is presented as a two-replica experiment (guard enforces).
- H100 vs MI350X uses real vLLM on both, explicitly labeled.
- Synthetic explicit-prefix locality is labeled as such; real prefix-cache reuse is shown separately.
