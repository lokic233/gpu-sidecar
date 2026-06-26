# Remaining Blockers — Round-5

## 1. Native request -> resident-block matching (UNCHANGED, honest)
Per-request matching of an incoming request to a runtime's resident KV blocks is STILL not trustworthy
on this stack. It requires all of:
- exact vLLM-version-specific block hashing (`ExternalBlockHash` over token_ids + extra_keys);
- the request's RAW token IDs (tokenization) — storing them violates the no-raw-token invariant;
- stable handling of extra_keys / cache_salt / multimodal / LoRA metadata across versions.

Status in code: `vllm_events` provider stays metadata-only — `match_supported=false`, NO matchable
native directory published, safe load-only fallback preserved, NO raw token IDs stored. Native KV
events ARE ingested (validated on real AMD MI350X + NVIDIA H100, identical schema) for aggregate
observability (index size, sequence health, KV headroom, reset epoch) only.

We did NOT pretend this is solved. "Native KV event ingestion validated"; "native per-request KV-block
matching remains unsupported".

## 2. Explicit locality is SYNTHETIC (by design, labeled)
The validated routing experiment uses explicit-prefix keys, each mapped to a REAL identical prompt
prefix (same key <-> same shared prompt text). This is a faithful controlled proxy for prefix reuse,
NOT native block matching. Real prefix-cache reuse is independently confirmed via each replica's
vllm:prefix_cache_hits_total delta, but the ROUTING decision uses the synthetic directory, not native
block residency.

## 3. Throughput separation on a tiny model
Qwen2.5-0.5B on H100 does not saturate at the tested loads, so policy throughput/latency differences
are small; the ASSIGNMENT behavior is the robust signal. A larger model / higher load would widen the
latency separation. This is a measurement-scope limitation, not a correctness gap.

## 4. process_start_time_seconds precision
vLLM exports it at ~12 sig figs, so two replicas launched within ~10ms can collide on
`runtime_instance_id`. Mitigated: `runtime_endpoint_id` (host+vllm-url+boot hash) is the PRIMARY
independence signal and is distinct per endpoint regardless of start time.

## 5. Backend service profiles are configured, not auto-calibrated
`--profiles` supplies static decode/prefill ms/token. No online profiler is built (out of scope).
A future calibrator can populate profiles without changing the policy.

## 6. Shared-box operational notes
- MI350X (devgpu499) is contended and has historically SIGABRT'd long-lived processes; the real ROCm
  vLLM and sidecar are kept running but may need restart for long sweeps.
- ROCm vLLM memory profiler can mis-estimate KV memory; launch with `--kv-cache-memory-bytes` to
  bypass (documented in the earlier audit addendum).

## Recommended interface for Liangqi's PPO (preserved, unchanged contract)
Per candidate backend, from ONE immutable snapshot generation:
  snapshot_generation, backend profile (decode/prefill ms-per-token, profile_fallback flag),
  queued_prefill / queued_decode / active_prefill / active_decode work, runtime running/waiting,
  KV pressure/headroom, prefix residency state (ABSENT/WARMING/READY), matched_prefix_tokens (only
  when READY), cache_confidence, cache_reset_epoch, est_queue_ms / est_prefill_ms / est_decode_ms,
  final_analytical_score_ms.
Learning: final_cost_i = analytical_estimated_finish_i + learned_residual_i. The analytical model
already encodes: cached prefixes reduce prefill; WARMING != READY; overloaded cache-hot nodes can
lose; stale/gapped cache is ignored. PPO learns only the residual.
