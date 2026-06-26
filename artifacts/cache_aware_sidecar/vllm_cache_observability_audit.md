# vLLM Cache-Observability Audit

**Scope:** exact cache-observation mechanisms available to this repository's sidecar on the two
runtimes actually deployed for E2E validation: NVIDIA **H100** (`devgpu014`) and AMD **MI350X**
(`devgpu499`). Findings are from live inspection of the installed runtimes on 2026-06-25, not from a
generic vLLM version.

---

## 0. Runtimes actually deployed (ground truth)

| Node | GPU | Inference runtime | Version | Prefix caching | Native KV events |
|---|---|---|---|---|---|
| `devgpu014` | NVIDIA H100 | **real vLLM** (`vllm.entrypoints.openai.api_server`) | **0.23.0** | yes (V1, on by default) | **yes** (`KVEventsConfig`, ZMQ) |
| `devgpu499` | AMD MI350X (gfx950) | **`mini_oai_server.py`** (custom HF `transformers` server) | n/a (transformers) | **no** | **no** |

> **Why MI350X is not vLLM:** the prebuilt vLLM wheel in `~/vllm-env` is CUDA-only; `import vllm`
> aborts on the MI350X host (native CUDA extension load failure). The team therefore runs a minimal
> OpenAI-compatible server backed by `AutoModelForCausalLM.generate` that exposes a **vLLM-style
> `/metrics` subset** (`vllm:num_requests_running`, `vllm:num_requests_waiting`,
> `vllm:kv_cache_usage_perc`=0.0, `vllm:generation_tokens_total`, `vllm:request_success_total`) so the
> sidecar runtime adapter parses both identically. It has **no paged KV cache, no automatic prefix
> caching, and no KV event publisher.**

This is the single most important finding: **the two runtimes expose materially incompatible cache
schemas.** Per the task's hard-stop guidance, this is precisely the situation that forces the
explicit-prefix experimental mode to be the trustworthy cross-vendor path, with native vLLM events
treated as an H100-only, metadata-only provider behind a documented blocker.

---

## 1. H100 / vLLM 0.23.0 — supported mechanisms

### 1a. Aggregate prefix-cache counters (always on, V1)
From live `GET /metrics` on `127.0.0.1:8000`:

```
# TYPE vllm:prefix_cache_queries_total counter
vllm:prefix_cache_queries_total{engine="0",model_name="..."} 12843.0
# TYPE vllm:prefix_cache_hits_total counter
vllm:prefix_cache_hits_total{...}            # cached tokens
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{...} 0.0
# TYPE vllm:num_requests_waiting_by_reason gauge   (capacity|deferred)
```

- **Aggregate-only.** `prefix_cache_hits_total / prefix_cache_queries_total` is a server-wide hit
  ratio **in tokens**. It says nothing about *which* request or *which* prefix is resident.
- **`kv_cache_usage_perc`** is a single gauge in `[0,1]` — usable as KV headroom (`1 - usage`).
- **`num_requests_waiting_by_reason`** (new in this version) splits the runtime waiting queue into
  `capacity` vs `deferred` — useful runtime pressure signal, still aggregate.

> **Rule honored:** the sidecar NEVER fabricates per-request prefix locality from these aggregate
> counters. They feed the runtime snapshot (`internal/runtime/vllm/metrics.go`) and the KV-headroom
> term only.

### 1b. Native KV block-lifecycle events (opt-in, ZMQ)
vLLM 0.23.0 ships a complete KV-event interface. Exact source paths:

- **Config:** `vllm/config/kv_events.py` → `KVEventsConfig{enable_kv_cache_events, publisher,
  endpoint, replay_endpoint, buffer_steps, hwm, max_queue_size, topic}`. Default
  `enable_kv_cache_events=False`; default endpoint `tcp://*:5557`.
- **Events:** `vllm/distributed/kv_events.py`:
  - `BlockStored{block_hashes:[ExternalBlockHash], parent_block_hash, token_ids:[int], block_size,
    lora_id, medium, lora_name, extra_keys, group_idx, kv_cache_spec_kind, kv_cache_spec_sliding_window}`
  - `BlockRemoved{block_hashes:[ExternalBlockHash], medium, group_idx}`
  - `AllBlocksCleared{}`
  - `KVEventBatch(EventBatch){ts:float, events:[...], data_parallel_rank}`
- **Emission point:** `vllm/v1/core/block_pool.py` enqueues `BlockStored` (line ~315, includes
  `token_ids=request.all_token_ids[start:end]`), `BlockRemoved` (~393), `AllBlocksCleared` (~493);
  `take_events()` (~518) drains the queue each scheduler step.
- **Transport:** `ZmqEventPublisher` — a `zmq.PUB` socket; each batch is sent as a **3-frame
  multipart**: `(topic_bytes, seq_bytes, msgpack_payload)` where `seq_bytes` is an **8-byte
  big-endian** monotone sequence (`itertools.count()`), and payload is `msgspec.msgpack`-encoded.
  An optional `zmq.ROUTER` **replay** socket answers `start_seq` requests from an in-memory
  `buffer_steps` ring (default 10 000), terminating with a `-1` end-of-sequence marker.
  At-least-once delivery + monotonic ordering are explicitly documented in the `EventPublisher` ABC.

#### Launch flags required to enable native events
```bash
vllm serve <model> --port 8000 \
  --kv-events-config '{"enable_kv_cache_events": true, "publisher": "zmq",
                       "endpoint": "tcp://*:5557", "topic": "kv@gpu3"}'
```
(or the equivalent `KVEventsConfig` in the Python API). **The H100 server currently running for E2E
was launched WITHOUT this flag** — events are disabled on the live server.

---

## 2. Request-specific vs aggregate vs unobtainable (H100 / vLLM 0.23)

| Information | Class | Notes |
|---|---|---|
| server-wide prefix-cache hit ratio (tokens) | **aggregate** | `prefix_cache_{hits,queries}_total`; no per-request attribution |
| KV cache usage | **aggregate** | `kv_cache_usage_perc` gauge → headroom |
| runtime waiting/running, waiting-by-reason | **aggregate** | runtime queue pressure |
| block stored/removed/cleared (which blocks resident) | **request-derived, via events** | requires `enable_kv_cache_events` + a ZMQ consumer |
| block→request attribution | **NOT reliably obtainable** | see §3 blocker |
| per-request `cached_tokens` in the OpenAI response `usage.prompt_tokens_details` | **request-specific IF present** | not exposed by the running config; not relied upon |

---

## 3. The native request-level matching BLOCKER (precise)

To answer *"how many prefix tokens of THIS incoming request are already resident on backend B"* using
native events, the sidecar would have to:

1. Subscribe to backend B's ZMQ PUB socket, decode msgpack `KVEventBatch`es, and maintain a block
   index keyed by `ExternalBlockHash`.
2. Compute, for the incoming request, the **same block hashes vLLM would compute** and test them
   against the index.

Step 2 is the blocker, for four independently sufficient reasons:

1. **Hash algorithm coupling.** `ExternalBlockHash` (`vllm/v1/core/kv_cache_utils.py`,
   `TypeAlias = bytes | int`) is produced by vLLM's internal block-hashing over **token IDs** plus
   `extra_keys` (MM identifiers, LoRA name, cache_salt, prompt-embedding hashes). Reproducing it in
   the Go sidecar means reimplementing — and version-pinning — vLLM-internal hashing. It is not a
   stable public contract; it has changed across vLLM versions.
2. **Raw token IDs required.** `BlockStored.token_ids` is the raw prompt token sequence. Matching a
   request to a block fundamentally needs the request's token IDs (i.e. tokenizing the prompt).
   **This violates invariant #8 (never persist token contents/user data).** We refuse to store them.
3. **Restart/epoch semantics unverifiable from outside.** On the running config events are off, so we
   could not observe a real restart's `AllBlocksCleared`/reconnect sequence to verify our
   gap/epoch handling against ground truth.
4. **Cross-vendor incompatibility.** MI350X emits **no** events at all, so a native-events design
   cannot be the cross-vendor contract — it would be H100-only.

**Conclusion:** native request-level cache matching is **NOT trustworthy on this stack.** The
`vllm_events` provider is therefore implemented as **metadata-only**: it ingests sanitized
block-lifecycle events into the bounded index (so the aggregate `/v1/cache` snapshot — index size,
sequence health, confidence, KV headroom — is real), but it reports `match_supported=false` and
returns **0 matched tokens at confidence 0** for every per-request `Lookup`. That makes the
cache-aware policy fall back to the load-only estimate for those requests — the required safe
behavior — instead of fabricating a match.

---

## 4. Fallback behavior when native KV events are unavailable

This is the default and the validated path:

- **Explicit-prefix mode** (`--cache-observer explicit`): locality is driven by an **opaque
  experiment key** carried on the request (`X-Cache-Prefix-Key`), hashed (SHA-256) before it is
  stored or emitted, never forwarded to vLLM. This is runtime-independent and therefore works
  **identically on H100 and MI350X**, giving deterministic, verifiable cache-aware routing for
  experiments. It is explicitly non-production.
- **Disabled mode** (default): no cache observation; `/v1/cache` reports `supported:false`; the
  cache-aware policy reduces exactly to the existing load-only estimate.
- **Any degradation** (event gap, runtime restart, stale snapshot, unsupported schema) →
  `cache_confidence=0`, `matched_prefix_tokens=0`, fall back to no-cache estimate (enforced in the
  index `confidence`/`isStale` logic and the policy `ConfidenceFloor`).

---

## 5. H100 vs MI350X runtime differences (summary)

| Dimension | H100 (vLLM 0.23.0) | MI350X (mini HF server) |
|---|---|---|
| paged KV / prefix caching | yes | **no** |
| `prefix_cache_*_total` | yes (aggregate) | **absent** |
| `kv_cache_usage_perc` | real gauge | hard-coded `0.0` (→ headroom always 1, honest given no paging) |
| native KV events (ZMQ) | yes (opt-in) | **none** |
| explicit-prefix mode | works | works (identical) |

The explicit provider papers over this gap **honestly**: it never claims the MI350X has a real KV
cache; it models locality at the experiment layer for both, which is exactly what a controlled
routing experiment needs.

---

## 6. What the sidecar actually does with each mechanism

| Mechanism | Used for | Where |
|---|---|---|
| `kv_cache_usage_perc` | KV headroom term in policy + `/v1/cache.kv_headroom` | runtime adapter → provider.SetKVHeadroom |
| `prefix_cache_*_total` | runtime snapshot only (NOT per-request locality) | `internal/runtime/vllm/metrics.go` |
| `generation_tokens_total` | **delta → service rate** (never used as a rate directly) | `internal/router/registry.go:serviceRate` |
| native KV events | metadata-only index (`vllm_events` provider), match unsupported | `internal/cache/vllm_provider.go` |
| explicit prefix key | deterministic per-request locality (hashed) | `internal/cache/explicit_provider.go` |

---

## 7. Verdict

- **Is native request-level cache matching trustworthy on this stack?** **No** — documented blocker
  (§3). Provider abstraction + safe fallback are complete; native matching is left as a clearly
  marked, unwired upgrade path.
- **Trustworthy, validated path:** explicit-prefix mode (cross-vendor, deterministic) + the
  load-only fallback. This is what the E2E results and the cache-aware analytical baseline are built
  on.
