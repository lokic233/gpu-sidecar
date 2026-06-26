# Cache-Aware Admission Sidecar — Architecture

This extends the existing GPU Host Sidecar (load observer + bounded admission proxy) into a
**local cache-and-capacity observer + conservative admission controller + materialized
cache-locality contract for the global router**, strictly additively and behind feature flags.

## Two observation planes (preserved split) + one new sub-plane

```diagram
                      ┌──────────────── one sidecar per GPU backend ────────────────┐
                      │                                                              │
 HW telemetry  ──────►│ OBSERVATION plane (existing): GPU telemetry, lifecycle,      │
 (slow loop)          │   stability, runtime /metrics scrape (incl. kv_cache_usage,  │
                      │   prefix_cache_*_total aggregate, generation_tokens_total)   │
                      │      │                                                       │
                      │      ├──► CACHE-OBSERVATION sub-plane (NEW, default off):     │
                      │      │      provider = disabled | explicit | vllm_events      │
                      │      │      bounded prefix INDEX (metadata only, no tokens)   │
                      │      │      KV headroom fed from runtime kv_cache_usage_perc  │
                      │      │      runtime-restart -> index reset (epoch bump)       │
                      │      ▼                                                        │
 client req ─────────►│ DATA plane (existing): admit -> bounded FIFO queue ->         │
 (hot path)           │   dispatch -> own vLLM conn -> transparent SSE relay          │
                      │      + explicit-prefix header extract/HASH/STRIP (NEW)        │
                      │      + optional token-level WORK accounting (NEW, additive)   │
                      │      + observe prefix locality on dispatch (NEW)              │
                      │                                                              │
                      │ HTTP: …existing… + GET /v1/cache (NEW)  + /metrics cache gauges│
                      └───────────────────────────────────────────────────────┬──────┘
                                                                                │ poll (off hot path)
   ┌────────────────────────────────────────────────────────────────────────┐ │
   │ Global Router Gateway (existing)                                          │◄┘
   │   registry materializes BackendState{…load…, +cache fields, +svc rate}    │
   │   + bounded per-backend cache DIRECTORY (hashed key -> matched tokens)     │
   │   policy: cache_aware_estimated_finish (NEW) reads materialized snapshot   │
   │           + O(1) local directory lookup — NO per-request network query     │
   │   emits full per-candidate RL state (CANDIDATE_STATE)                      │
   └────────────────────────────────────────────────────────────────────────┘
```

## Invariants preserved (mapping to task §0)

1. **Router owns global selection** — unchanged; policy is still pure over a materialized snapshot.
2. **Sidecar owns local admission/dispatch/observation** — cache sub-plane lives in the sidecar.
3. **vLLM owns KV/prefix caching/batching** — we only *observe*; never manage blocks.
4. **No synchronous telemetry scrape on hot path** — the cache directory is materialized by the
   registry's background loop; the policy does an O(1) in-memory map lookup. `/v1/cache` is polled,
   never scraped per request.
5. **Observation-plane failure never blocks streaming** — provider runs on its own goroutines; the
   relay path never calls the provider synchronously except the cheap `Observe()` (a lock-bounded
   map write) at dispatch.
6. **Admission queue ≠ vLLM scheduler queue** — unchanged; work accounting is additive to the
   request-count/inflight hard bounds.
7. **SSE/cancel/retry/drain/trajectory** — unchanged; new `CANDIDATE_STATE` event added.
8. **Never log/persist raw prompts/responses/tokens/user data** — the explicit key is SHA-256
   hashed before storage/emission; native `token_ids` are dropped at the transport boundary and
   never enter Go; `/v1/cache` exposes only counts + hashed directory keys.
9. **Unsupported features marked unsupported** — providers report `supported`/`match_supported`;
   the native provider reports `match_supported=false` (blocker) rather than a fake zero.
10. **No sidecar-to-sidecar comms** — providers ingest only their own local runtime's events;
    the router (control plane) is the only aggregator.

## New components

| Path | Role |
|---|---|
| `internal/cache/model.go` | Mode, PrefixQuery, MatchResult, Snapshot, IndexEntry |
| `internal/cache/index.go` | bounded thread-safe prefix index (seq/gap/dup/TTL/stale/evict/reset/directory) |
| `internal/cache/provider.go` | Provider interface + HashKey + DisabledProvider |
| `internal/cache/explicit_provider.go` | deterministic explicit-prefix provider (cross-vendor) |
| `internal/cache/vllm_provider.go` | native-events provider (metadata-only, match-unsupported) |
| `internal/cache/http_snapshot.go` | `/v1/cache` body (snapshot + bounded directory) |
| `internal/dataplane/work.go` | optional token-level work accountant (additive) |
| `internal/router/policy_cache_aware.go` | `cache_aware_estimated_finish` + `cache_affinity_only` |

## Design choice for the router contract (task §4)

We chose **Design 1**: the sidecar publishes a bounded cache directory through `GET /v1/cache`, and
the router registry materializes it off the hot path (alongside the existing load snapshot). The
policy then does an O(1) local map lookup per request. This honors *"the router reads
already-materialized in-memory state and does not synchronously query every sidecar per request"* and
avoids any O(backends) network query in the routing decision. A synchronous lookup endpoint is **not**
added (we did not need a debug one). The native ZMQ transport is **not** the default and is left
unwired (documented blocker).
