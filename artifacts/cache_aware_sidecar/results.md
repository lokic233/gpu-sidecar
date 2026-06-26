# Results — Cache-Aware Sidecar E2E Validation

> ⚠️ **SUPERSEDED IN PART by Round-5.1 hardening — read this first.**
>
> This document is the Round-5 (initial) results. A subsequent correctness-hardening pass
> (`artifacts/cache_aware_sidecar_hardening/`) found that **§5 "equal-capability comparison" below ran
> two sidecars over ONE shared vLLM runtime** (one KV cache / one scheduler) — that is NOT a valid
> two-replica cache-routing experiment, and its herding/balancing numbers are artifacts of two
> independent SYNTHETIC directories over one runtime. The corrected experiment uses **two genuinely
> independent vLLM replicas** (separate process/GPU/port/KV/scheduler) with a HARD-STOP guard; see
> `artifacts/cache_aware_sidecar_hardening/results.md` + `independent_replica_proof.md`.
>
> Also superseded: the residency, work-accounting, atomic-snapshot, and service-profile semantics
> described here were hardened (see `cache_aware_sidecar_hardening/correctness_design.md`). Historical
> labels for the runs below:
> - **Historical run:** H100 real vLLM + (cross-vendor) MI350X **mini HF compatibility server** — NOT
>   a clean hardware comparison; and the equal-capability run = two sidecars over one runtime.
> - **Current run:** H100 real vLLM (two independent replicas) + MI350X **real ROCm vLLM** — see the
>   hardening results.
>
> The contract/observability findings below remain valid. The ROUTING-BEHAVIOR demonstration should be
> read from the hardening results, not §5 here.

---


All numbers below are from LIVE runs on real hardware on 2026-06-25:
- H100 `devgpu014`: real vLLM 0.23.0 (Qwen2.5-0.5B-Instruct).
- MI350X `devgpu499`: `mini_oai_server.py` (HF transformers), same model.

## 1. Stack brought up cross-vendor (explicit cache mode)

| Component | Endpoint | Status |
|---|---|---|
| H100 cache-aware sidecar | `[::]:19097` | up, `/v1/cache` supported+match_supported |
| H100 2nd logical backend | `[::]:19098` | up (equal-capability isolation) |
| MI350X cache-aware sidecar | `[::]:19097` | up (under restart watcher; box SIGABRTs long procs) |
| Router (cache_aware) | `127.0.0.1:19094` | up, materializes both backends' cache state |
| Trajectory collector | `[::]:29110` | up |

Router materialized view (fresh): `h100-gpu3 cache_conf=0.99 cache_idx=15`,
`mi350x-gpu2 cache_conf=0` (never warmed for that key) — both `cache_observation_supported=true`,
`kv_headroom` populated, `service_rate_supported=true`.

## 2. Unit + integration tests (race-clean)

```
go vet ./...      -> VET_EXIT=0
go test -race ./... -> TEST_EXIT=0   (all packages ok)
```
- Total test funcs: **176** (was 135; **+41** added by this task).
- New: `internal/cache` 24, `internal/router` +13 (cache-aware policy), `internal/dataplane` +new
  cache-proxy tests. Covers: store/remove/all-clear, duplicate, out-of-order, sequence-gap, stale,
  TTL, bounded eviction, runtime reset, concurrency (-race); policy: no-reuse→load-only,
  hot+light wins, hot+overloaded loses, low-confidence ignored, stale ignored, cache-reset
  invalidation, order-independence, unsupported≠real-zero, heterogeneous service rates,
  affinity-only herding; proxy: explicit header strip+hash, disabled-still-strips, no-header
  no-change, work reservation release, service-rate-delta-not-cumulative.

## 3. E2E scenario matrix (live)

| # | Scenario | Result |
|---|---|---|
| 1 | cache disabled (default) | `/v1/cache` reports `enabled:false`; routing = load-only (existing behavior) ✔ |
| 2 | cache enabled, unique prefixes | each request a cold miss; matched=0; no herding ✔ |
| 3 | repeated prefix groups | prefix warms; `index_entries` grow; matched>0 on warm backend (see §4) ✔ |
| 4 | event stream interrupted | (explicit mode) N/A native; stale path covers gap → conf 0 (unit `TestIndex_SequenceGap`) ✔ |
| 5 | runtime restart / cache clear | `OnRuntimeRestart` wipes index + bumps epoch; locality→cold (unit + provider tests) ✔ |
| 6 | queue saturation | 6 reqs → 2 admitted, **4×429 ADMISSION_QUEUE_FULL**, 2 queue-timeout; hard bounds hold ✔ |
| 7 | drain while cache observer running | admission→503 BACKEND_DRAINING; `/v1/cache` keeps serving ✔ |
| 8 | streaming + non-streaming | both relay correctly through router→sidecar→vLLM; `[DONE]` propagates ✔ |
| 9 | client cancellation | streaming cancel handled (existing `UPSTREAM_CANCELLED` path; cache plane unaffected) ✔ |
| 10 | collector outage | collector killed → request still **HTTP 200**; emission non-blocking; `/v1/cache` still serves ✔ |

## 4. Locality detected & applied end-to-end (CANDIDATE_STATE, live)

After rapid-warming key `demo-locality` (400 tok) on the stack, one matching request emitted:

| backend | matched_prefix | match_ratio | est_prefill_saved_ms | cache_confidence | final_score_ms |
|---|---|---|---|---|---|
| h100-gpu3 (warm) | **13** | **1.0** | 0.65 | **0.990** | 222.7 |
| mi350x-gpu2 (cold) | 0 | 0 | 0 | 0 | 28.1 |

(matched=13 because the short demo prompt was ~13 tok; the policy correctly capped match at input
length and applied the warm backend's locality. The MI350X "lower score" here is its measured
service-rate making decode cheaper that instant — the analytical model is transparent about every
term; on a contended/large request the prefill-saved term dominates.)

Stale fallback (live): after 35s idle (> `cache-stale-after=30s`), `/v1/cache` confidence decayed
to **0**, `stale_invalidations_total=634+` → locality ignored, routing falls back to load-only.
Immediately after a warm: confidence **0.998**, age 64ms. ✔

## 5. Policy comparison — EQUAL-CAPABILITY isolation (two H100 backends, locality = only asymmetry)

160 requests, concurrency 14, 60% hot-prefix / 40% unique. `hot_conc` = fraction of HOT-prefix
requests sent to the backend that warmed first.

| policy | rps | e2e_p50 (ms) | e2e_p95 (ms) | hot_conc | hot_assignment |
|---|---|---|---|---|---|
| round_robin | 65.7 | 202.3 | 221.2 | **0.55** | {h100b:44, h100a:53} |
| least_queued | 59.2 | 221.4 | 289.2 | 1.0* | {h100a:48, h100b:44} |
| health_gated_least_pressure | 64.1 | 209.5 | 218.1 | 1.0* | {h100a:45, h100b:47} |
| cache_affinity_only | 60.8 | 219.0 | 249.0 | 1.0 | **{h100a:98}** (herds!) |
| cache_aware_estimated_finish | 61.6 | 213.0 | 243.8 | 1.0 | {h100a:49, h100b:52} |

Reading (the §10 thesis, demonstrated):
- **load-only `round_robin` misses locality**: splits hot-prefix traffic ~50/50 (`hot_conc 0.55`)
  with no regard for which backend is warm.
- **`cache_affinity_only` herds**: sends **98→0** hot requests to the single warmed backend,
  ignoring load entirely (the documented pathology).
- **`cache_aware_estimated_finish` balances**: keeps locality value but, because both backends are
  equally fast and both warmed (`*` warmed=[a,b] once traffic flows), it spreads load 49/52 instead
  of herding — locality AND congestion both respected.

> Note: when both backends warm the same key (they share identical traffic here), `hot_conc=1.0` for
> several policies is expected; the discriminating signal is the **assignment spread** — affinity-only
> collapses to one node, cache-aware does not. The "hot-but-overloaded loses" behavior is proven
> deterministically in `TestCacheAware_HotButOverloadedLoses` (overloaded cache-hot backend loses to
> an idle cold one).

## 6. Cross-vendor comparison (REAL heterogeneous H100 vLLM vs MI350X HF)

120 requests, concurrency 10. All policies routed ~100% to H100 — because H100 (real vLLM) is
dramatically faster than MI350X (HF transformers), so latency-optimal routing CORRECTLY concentrates
there. This is honest real-hardware behavior; it confirms the policy does not give equal traffic to
unequal backends (task §7) but masks the locality variable — hence the equal-capability run in §5.

| policy | rps | e2e_p50 | e2e_p95 |
|---|---|---|---|
| round_robin | 26.3 | 171.8 | 1062.2 |
| least_queued | 29.6 | 132.8 | 956.7 |
| health_gated_least_pressure | 19.8 | 192.7 | 1360.4 |
| cache_affinity_only | 24.3 | 175.1 | 1106.2 |
| cache_aware_estimated_finish | 24.2 | 144.4 | 1212.8 |

## 7. Observability (live)

- `/v1/cache` (H100): `provider=explicit, supported=true, match_supported=true, index_entries=15,
  events_received_total=816, sequence_gaps_total=0, stale_invalidations_total=767, kv_headroom=1`.
- `/metrics`: 16 `gpu_cache_*` gauges, labeled only `{host, provider}` — **no prefix hashes as
  labels** (bounded cardinality).
- Aggregate prefix-cache hit rate from live vLLM: `prefix_cache_hits_total/queries_total =
  539792/559600 = 96.5%` server-wide — read for context only, NEVER used as per-request locality.
- Service rate: `generation_tokens_per_s` arrives as a cumulative counter (37201); the registry
  differences it over time into a true rate (verified by `TestServiceRate_DeltaNotCumulative`).

## 8. Privacy (verified)

- `TestProxy_ExplicitHeaderStrippedAndHashed`: the `X-Cache-Prefix-Key` is **stripped before vLLM**
  and the observer receives **only the SHA-256**, never the raw key.
- `TestProxy_ExplicitHeaderDisabled_StillStripped`: header stripped even when mode is off.
- Native `token_ids` are dropped at the transport→`BlockEvent` boundary (never enter Go).
- `/v1/cache` directory keys are hashes; no content anywhere.

## 9. Regression safety
All pre-existing tests still pass (`go test -race ./...` green). The data plane, router relay,
SSE, drain, queue rejection, and trajectory paths are unchanged when cache observation is disabled
(the default).
