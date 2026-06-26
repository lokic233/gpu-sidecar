# Implementation Report — Cache-Aware Admission Sidecar

## Summary

Extended the GPU Host Sidecar from *(local load observer + bounded admission proxy)* into
*(local cache-and-capacity observer + conservative admission controller + materialized
cache-locality contract for the global router)* — strictly additively, behind feature flags that
default OFF. The router (not the sidecar) still owns global backend selection; the sidecar (not the
router) still owns local KV-adjacent observation. vLLM still owns actual KV/prefix caching.

The headline engineering decision, forced by a live audit: **native vLLM KV-event request matching
is NOT trustworthy on this stack** (raw-token requirement + version-coupled hashing + MI350X has no
vLLM at all), so the trustworthy, validated path is a **deterministic explicit-prefix provider** plus
a **strong analytical cache-aware routing baseline**, with native events implemented as a
metadata-only provider behind a precisely documented blocker. This is the honest outcome the task's
hard-stop conditions call for — not a mocked "cache-aware" claim.

## Files changed / added

### New (`internal/cache/`)
- `model.go` — Mode, PrefixQuery, MatchResult, Snapshot, IndexEntry.
- `index.go` — bounded thread-safe prefix index: store/remove/all-clear, duplicate/out-of-order/gap
  detection, TTL, staleness→confidence decay, bounded eviction, reset/epoch, bounded Directory().
- `provider.go` — Provider interface, HashKey (SHA-256), DisabledProvider, NewProvider factory.
- `explicit_provider.go` — deterministic explicit-prefix provider (cross-vendor; Observe/Lookup).
- `vllm_provider.go` — native KV-events provider (metadata-only; `match_supported=false`; EventSource
  abstraction + sanitized BlockEvent that drops raw token_ids).
- `http_snapshot.go` — `/v1/cache` body (Snapshot + bounded hashed directory).
- `index_test.go` (12), `provider_test.go` (12) — unit tests.

### New (`internal/router/`, `internal/dataplane/`)
- `internal/router/policy_cache_aware.go` — `cache_aware_estimated_finish` (documented coefficients)
  + `cache_affinity_only` baseline + CandidateScore breakdown + CacheLocator.
- `internal/router/policy_cache_aware_test.go` (13) — routing-policy behavior tests.
- `internal/dataplane/work.go` — optional token-level WorkAccountant (additive to hard bounds).
- `internal/dataplane/cache_proxy_test.go` — explicit-header strip/hash + work-accounting tests.

### Modified
- `internal/router/policy.go` — RequestFeatures gains PrefixKeyHash/PrefixTokens/CacheEligible/
  SessionKeyHash; PolicyByNameWithLocator registers cache policies.
- `internal/router/registry.go` — BackendState gains cache-locality + service-rate fields; registry
  polls `/v1/cache`, materializes a bounded per-backend directory off the hot path, and computes
  service rate as a counter delta (`serviceRate`).
- `internal/router/gateway.go` — extracts+hashes prefix headers into features, propagates them to the
  sidecar, emits `CANDIDATE_STATE` with the full per-candidate RL state.
- `internal/dataplane/proxy.go` — extract/HASH/STRIP explicit headers, observe locality on dispatch,
  optional work reservation/activate/release.
- `internal/api/server.go` — `GET /v1/cache` + 16 bounded-cardinality `gpu_cache_*` metrics.
- `cmd/sidecar/main.go` — cache flags, provider wiring, KV-headroom feeder + runtime-restart
  detection, combined `/v1/queue` with work accounting.
- `cmd/router/main.go` — uses the registry as the cache locator.
- `experiments/` — `cache_harness.py`, `cache_compare.py`, `cache_compare_equal.py`.
- `scripts/watch_mi350x_cache_sidecar.sh` — restart watcher for the contended MI350X box.

## Architectural decisions

1. **Provider abstraction** (disabled | explicit | vllm_events) so the trustworthy path ships now and
   native events can be upgraded later without touching the router or policy.
2. **Router contract = Design 1** (task §4): sidecar publishes a bounded cache directory via
   `/v1/cache`; the registry materializes it off the hot path; the policy does an O(1) local map
   lookup. No O(backends) network query in any routing decision. No synchronous telemetry scrape on
   the hot path.
3. **Analytical baseline with explicit coefficients** (no magic constants): every term
   (prefill/decode/queue/staleness/KV-pressure) is a named, defaulted, documented field in
   `CacheAwareConfig`. The score is the base over which PPO learns a residual.
4. **Service rate fixed at the source**: the runtime exposes `generation_tokens_total` as a
   cumulative counter; the registry differences it over wall time. Cumulative totals are never used
   as rates.
5. **Conservative work accounting**: high confidence reserves on uncached prompt tokens; low/stale
   confidence reserves on full prompt tokens. Additive to — never a replacement for — the existing
   request-count/inflight hard bounds.
6. **Order-independent selection**: ties broken by logical backend id, so snapshot iteration order
   never changes the chosen backend (tested).

## Exact cache provider used (for E2E)
`explicit` (deterministic, cross-vendor). Validated live on H100 (real vLLM 0.23) and MI350X (HF
server). `vllm_events` is implemented and unit-tested but intentionally **not** wired to a ZMQ
transport (blocker below); `disabled` is the default.

## Unsupported features (explicitly marked, not faked)
- Native per-request prefix→block matching: `match_supported=false` on this stack (blocker §below).
- MI350X has no prefix cache / no KV events / `kv_cache_usage_perc`≡0 (HF server) — KV headroom is
  honestly 1 there.
- vLLM `prefix_cache_*_total` is aggregate-only; never used for per-request locality.

## Privacy / security decisions
- `X-Cache-Prefix-Key` SHA-256 hashed in the proxy before storage/emission; raw value never leaves
  the extract function; header stripped before forwarding to vLLM (even when mode disabled).
- Native `BlockStored.token_ids` dropped at the transport→BlockEvent boundary; raw token ids never
  enter Go or the index.
- `/v1/cache` exposes only counts + hashed directory keys; `/metrics` uses no hashes as labels.
- No prompts/responses/token contents logged or persisted by any cache-plane path. LogContent
  defaults false (unchanged).
- All cache features default OFF.

## Fallback semantics
On event gap, runtime restart, stale snapshot (age > `cache-stale-after`), unsupported schema, or
confidence < floor: `cache_confidence→0`, `matched_prefix_tokens→0`, policy uses the load-only
estimate. A cache-hot but overloaded backend can lose; a stale/unsupported observation never behaves
as a real zero. All verified by unit tests + live (§stale fallback decayed to conf 0 after 35s).

## Tests executed and results
- `go vet ./...` → clean.
- `go test -race ./...` → all packages OK, race-clean. 176 test funcs (+41).
- Live E2E: 10-scenario matrix (cache disabled/unique/repeated/interrupt/restart/saturation/drain/
  streaming+non-streaming/cancellation/collector-outage) — all pass (see results.md §3).
- Live locality applied end-to-end via CANDIDATE_STATE (matched=13, conf=0.99 on warm backend).
- Live policy comparison (equal-capability isolation + cross-vendor) — see results.md §5–6.

## Benchmark commands
```bash
# unit + race
go test -race ./... ; go vet ./...
# equal-capability policy comparison (cache locality = only asymmetry)
REQS=160 CONC=14 python3 experiments/cache_compare_equal.py
# cross-vendor comparison (real H100 vLLM vs MI350X HF)
REQS=120 CONC=10 ARRIVAL=steady python3 experiments/cache_compare.py
# single-policy load
python3 experiments/cache_harness.py --router http://127.0.0.1:19094 --requests 160 --concurrency 12
```

## Remaining blockers for real H100 + MI350X validation
1. **Native KV-event request matching (H100)**: requires (a) reproducing vLLM 0.23's internal
   block-hash over token_ids+extra_keys in Go AND (b) tokenizing the request prompt to compute its
   block hashes — which means handling raw tokens (privacy invariant violation). Blocked by design,
   not by effort. The transport (ZMQ + msgpack) and replay/seq semantics are fully documented in the
   audit; a future verified matcher flips `match_supported=true` and fills `Directory()`/`Lookup()`.
   It would also need libzmq + a msgpack dep (the repo is currently stdlib-only).
2. **MI350X has no vLLM** (CUDA-only wheel): real prefix-cache validation on AMD is impossible until
   a ROCm vLLM build exists. The explicit provider is the correct stand-in.
3. **MI350X box contention**: the host externally SIGABRTs long-lived processes (load ~20-27); a
   restart watcher is required for sustained runs. Does not affect cache correctness (identical
   binary stable on H100).
4. **Running H100 vLLM launched without `--kv-events-config`**: to exercise the native path end to
   end, relaunch vLLM with `enable_kv_cache_events=true, publisher=zmq` and build the ZMQ consumer.

## Recommended interface for Liangqi's PPO
- State = per-candidate `CANDIDATE_STATE` numeric fields (load + runtime + service rate + cache +
  analytical breakdown). The analytical terms are strong priors — feed them in.
- `final_score = final_analytical_score_ms + learned_residual`; select argmin.
- Reward = any function of outcome candidates (e.g. -e2e_latency_ms / SLO-shaped). Do NOT reward
  equal utilization or raw hit rate.
- The cache fields are exactly the previously-missing locality state that made identical load-only
  states yield different latencies — including them removes the hidden variable that destabilized PPO.

## Is native request-level cache matching trustworthy?
**No** on this stack (blocker #1). The provider abstraction, explicit experimental mode, and safe
fallback are complete and validated; the native integration blocker is documented precisely. The
result is honest and usable, not a mocked cache-aware claim.
