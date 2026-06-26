# Cache-Aware RL State Contract

The exact state every routing decision emits so Liangqi's PPO can reconstruct the (observation,
action, analytical base score, outcome) tuple per candidate backend. This EXTENDS the existing
`artifacts/e2e_vllm_flow/rl_trajectory_contract.md` — it does not replace it.

## Design principle

```
final_score = analytical_cache_aware_score  +  learned_residual
              ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^      ^^^^^^^^^^^^^^^^^
              this task (strong baseline)        Liangqi's PPO (NOT this task)
```

The analytical `cache_aware_estimated_finish` cost is emitted per candidate as the **base score**.
PPO learns only the unexplained residual over it. We do NOT implement PPO here.

## New event: `CANDIDATE_STATE` (router, per candidate backend, at decision time)

Emitted for EVERY candidate backend on every request (join key `request_id`). All fields are
metadata; no prompt/response/token-ids/unhashed keys.

### Load / runtime (observation)
| field | meaning |
|---|---|
| `queue_depth`, `queue_inflight`, `queue_max` | host admission queue (distinct from runtime queue) |
| `runtime_running`, `runtime_waiting` | vLLM runtime scheduler counts |
| `kv_cache_util`, `kv_headroom`, `kv_headroom_supported` | KV pressure / headroom |
| `lifecycle_state`, `stability_score`, `snapshot_age_ms` | backend health + freshness |
| `eligible` | whether this backend was routable |

### Service rate (delta-derived — NOT cumulative)
| field | meaning |
|---|---|
| `gen_tokens_per_sec` | generation tokens/s = Δ`generation_tokens_total` / Δt (registry) |
| `service_rate_supported` | false until two consecutive supported scrapes exist |

> The runtime exposes `generation_tokens_total` as a **cumulative counter**. The registry differences
> it over wall time (`serviceRate`). The raw total is NEVER used as a rate.

### Cache observation
| field | meaning |
|---|---|
| `cache_observation_supported` | provider can observe locality at all |
| `cache_match_supported` | per-request matching is trustworthy (false for native on this stack) |
| `cache_confidence` | [0,1]; 0 ⇒ stale/gap/unsupported ⇒ locality ignored |
| `cache_ready`, `cache_snapshot_age_ms` | freshness |
| `cache_event_sequence`, `cache_reset_epoch`, `cache_index_size` | event/epoch/size health |
| `matched_prefix_tokens` | matched prefix length for THIS request on THIS backend |
| `match_ratio` | matched / input_len_est |
| `uncached_prompt_tokens` | max(0, input_len_est - matched) |

### Analytical cost breakdown (the base score)
| field | meaning |
|---|---|
| `est_queue_ms` | queue_delay term |
| `est_prefill_ms` | prefill(uncached_prompt_tokens) |
| `est_decode_ms` | decode(expected_output) using measured service rate when available |
| `cache_staleness_penalty_ms` | penalty when supported but low-confidence (×(1-conf)) |
| `kv_pressure_penalty_ms` | penalty ×(1-kv_headroom) when measurable |
| `estimated_prefill_saved_ms` | matched_prefix_tokens × prefill_ms_per_token |
| `final_analytical_score_ms` | the base score = sum of the above (lower=better) |
| `cache_used` | whether locality affected this candidate |

## Request features (router `REQUEST_RECEIVED`, extended)
| field | meaning |
|---|---|
| `cache_eligible` | request carries a usable prefix key |
| `prefix_key_hash` | SHA-256 of the opaque key (NEVER raw) |
| `prefix_tokens` | claimed reusable prefix length |
| (existing) `input_len_est`, `model`, `stream`, `slo_class` | request shape |

`RequestFeatures` also carries `SessionKeyHash` (hashed) reserved for future session affinity.

## Outcome (existing reward candidates, unchanged + cache additions)
After completion, per `request_id` (from existing router/sidecar events):
- `client_observed_ttft_ms`, `e2e_latency_ms`, `sidecar_queue_delay_ms`, `local_vllm_ttft_ms`
- `completed` / `partial_stream_failed` / `rejected` / `queue_timeout` / cancellation
- `retry_cost`, output tokens (→ tokens/s)
- cache-related actuals when available: the sidecar `QUEUE_DEQUEUED` carries `queue_wait_ms`; the
  runtime aggregate `prefix_cache_hits_total/queries_total` can be sampled for a server-wide hit
  ratio (NOT per-request — see audit §1a).

## Recommended PPO interface (for Liangqi)

1. **State vector per candidate**: concat the CANDIDATE_STATE numeric fields above (load, runtime,
   service rate, cache, and the analytical breakdown). The analytical terms are highly informative
   priors — include them as input features.
2. **Base score**: use `final_analytical_score_ms` directly as the action value baseline; learn a
   residual `r_i` per candidate; select `argmin_i (final_analytical_score_ms_i + r_i)`.
3. **Reward**: any function of the outcome candidates (e.g. `-e2e_latency_ms`, or an SLO-shaped
   reward). Do NOT reward equal GPU utilization or raw cache-hit rate (task §7).
4. **Safety**: when `cache_confidence < floor` or `cache_match_supported=false`, the base score
   already ignores locality — the residual should learn nothing spurious from those rows.
5. **Partial observability fix**: the cache fields are exactly the missing locality state that made
   identical visible (load-only) states produce different latencies. Including them removes the
   hidden variable that destabilized PPO.

## Privacy
No prompts/responses/token-ids/unhashed keys are ever emitted. Prefix/session keys are SHA-256
hashed. Directory keys (in `/v1/cache`) are hashes. This is enforced in `proxy.go` (hash+strip),
`gateway.go` (hash), and the cache index (metadata-only).
