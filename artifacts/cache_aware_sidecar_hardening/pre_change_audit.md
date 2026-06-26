# Pre-Change Audit — Round 5 Cache-Aware Hardening

Verified against CODE and raw artifacts on 2026-06-26 (branch `navi/cache-hardening`, base commit
`7034f4b`). README claims are NOT taken as ground truth; every line below was checked in source.

## What is IMPLEMENTED (exists and compiles, 178 tests green at base)
- Global router gateway (`internal/router/gateway.go`), per-host data plane (`internal/dataplane/`),
  bounded admission queue (`queue.go`), vLLM runtime adapter (`internal/runtime/vllm/`).
- Cache provider abstraction (`internal/cache/provider.go`): `disabled | explicit | vllm_events`.
- Explicit-prefix provider (`explicit_provider.go`), metadata-only native provider
  (`vllm_provider.go`, `match_supported=false`).
- Cache-aware analytical policy (`policy_cache_aware.go`: `cache_aware_estimated_finish`,
  `cache_affinity_only`), token work accounting (`work.go`), `CANDIDATE_STATE` emission.
- `/v1/cache` + `gpu_cache_*` metrics; explicit-prefix header hash+strip; KV-headroom feed.

## What is EXPERIMENTALLY VALIDATED (real, defensible)
- Real vLLM serves on BOTH H100 (vLLM 0.23.0, GPU telemetry confirmed) and MI350X (real ROCm vLLM
  0.21.1, `/v1/models` 200, real prefix_cache counters climbing). [commits 332589d/0b3211f]
- Native vLLM KV events captured live on MI350X over ZMQ; schema identical to NVIDIA; raw token_ids
  present in `BlockStored` on both. 12 real sanitized AMD events replay through the provider
  (`vllm_real_amd_test.go`, passing).
- Runtime adapter parses real vLLM 0.23 + 0.21 `/metrics` correctly (verified via ParseForTest).
- Data-plane behaviors: SSE relay, cancellation, drain→503, queue-full→429, collector-outage→still-200
  (live + unit tests).

## What is SYNTHETIC ONLY (must be labeled; NOT a real cache-routing demonstration)
- **The equal-capability policy comparison (`cache_compare_equal.py`, results.md §5) ran two sidecars
  (`:19097`, `:19098`) over ONE shared vLLM runtime.** Its own docstring says "both H100 sidecars ->
  same fast vLLM". This is INVALID as a two-replica experiment: the two "backends" share one KV cache,
  one scheduler, one continuous batch, one runtime queue. The 98→0 affinity-herding / cache-aware
  balancing numbers are artifacts of two *independent synthetic directories* over one runtime, NOT
  two inference replicas. (P0 #2)
- Explicit-prefix locality is synthetic: `X-Cache-Prefix-Key` labels (e.g. "hotgroup-A", "ISOHOT")
  are NOT tied to identical real prompt-token prefixes. The hot/warm prompts in the harness are
  short filler, so vLLM's REAL prefix cache barely overlaps with the synthetic key grouping. (P0 #8)
- The cross-vendor comparison (`cache_compare.py`, results.md §6) compared H100 real vLLM vs MI350X
  **mini HF server** — not a clean hardware comparison. (labeling, §10)

## What remains BLOCKED (honest)
- Native per-request request→resident-block matching: needs vLLM-version-specific block hashing +
  raw token IDs + extra_keys/cache_salt/MM/LoRA handling. Unsolved; `vllm_events` stays metadata-only,
  `match_supported=false`, no matchable native directory. (correct today; must STAY this way — §8)

## CLAIMS TOO STRONG or INTERNALLY INCONSISTENT (to fix)
1. **`Observe()` marks a prefix cached at DISPATCH** (`explicit_provider.go` + `proxy.go`): a prefix
   becomes a reusable "warm" hit before prefill completes. A request that fails pre-first-token, is
   cancelled, or hits a runtime restart leaves a FALSE-POSITIVE cache entry. No WARMING state. (P0 #3)
2. **Work accounting reserves at DISPATCH, not at queue admission** (`proxy.go`): documented semantics
   ("reservations include queued and active work") contradict the code (queued requests reserve
   nothing). Also recomputed independently rather than tied to the ticket. Counters lack the
   queued/active/outstanding split the docs imply. (P0 #4)
3. **Routing state and cache directory are NOT atomically consistent** (`registry.go`): `r.snap`
   (BackendSnapshot) and `r.cacheDir` are two separate stores; the policy reads `b` from the snapshot
   but calls `locator.LookupPrefixTokens()` against the LIVE `r.cacheDir`. A refresh between those two
   reads mixes new backend confidence with an old/new directory across generations. (P0 #5)
4. **Aggregate throughput used as per-request decode speed** (`policy_cache_aware.go:decodeMsPerToken`
   = `1000/GenTokensPerSec` where `GenTokensPerSec` = Δgeneration_tokens_total/Δt): under continuous
   batching a busier backend reports higher aggregate throughput → policy thinks it's faster → sends
   more → positive feedback / herding. No backend service profile separate from live congestion. (P1 #6)
5. **Cumulative gap counter as a permanent soft confidence penalty** (`index.go:confidenceLocked`:
   `0.25 * sequenceGaps`): a single historical gap permanently degrades confidence forever, even after
   recovery. No `unresolved_gap` trust state; no zeroing on unresolved gap. (P1 #7)
6. **`stale_invalidations_total` increments on EVERY `/v1/cache` poll while stale**
   (`index.go:SnapshotMeta`), not once per fresh→stale transition (observed live: 634→767 from
   polling, not real invalidations). (P1 #7)
7. **No per-entry sequence ordering** (`index.go:ApplyRemove`/`ApplyStore`): only the global batch
   seq is checked; an old remove (valid batch seq, late delivery) can clear a NEWER store for the same
   key, and vice versa. (P1 #7)
8. README/results say cache-aware routing "balances" / affinity "herds" as if demonstrated on real
   replicas — actually shown only on the invalid shared-runtime experiment. Wording overclaims. (§10)

## Feasibility for the required fixes (checked live)
- H100 (`devgpu014`): 8× H100, GPUs 0/1/4/6/7 idle. **Two independent vLLM 0.23 replicas on two
  separate GPUs is feasible** → valid equal-capability experiment.
- MI350X (`devgpu499`): 8× gfx950, mostly idle, real ROCm vLLM 0.21.1 works → heterogeneous run with
  real vLLM on both vendors is feasible.

## Invariants to preserve (all currently hold; must remain after fixes)
Router owns selection; sidecar owns local admission/observation; vLLM owns KV; no hot-path scrape;
cache failure never blocks inference; sidecar queue ≠ vLLM queue; SSE/cancel/drain/retry/collector
regression-free; never log raw prompts/tokens/keys; unsupported≠zero; cache OFF by default; no
sidecar-to-sidecar comms. Incremental changes only.
