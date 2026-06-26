# Native KV-Event Evidence — REAL vLLM on AMD MI350X (gfx950, ROCm 7.0.2)

Captured live 2026-06-25 from a real vLLM source build (`~/vllm-src`, v0.21.1rc1) running on
MI350X GPU, launched with `--kv-events-config '{"enable_kv_cache_events":true,"publisher":"zmq",
"endpoint":"tcp://*:5557","topic":"kv@mi350x"}'`. A ZMQ SUB client connected to tcp://127.0.0.1:5557
and decoded msgpack KVEventBatch frames.

## CORRECTION to the earlier audit
The earlier audit stated AMD exposes NO KV events and that AMD/NVIDIA schemas are "materially
incompatible." That was true ONLY of the placeholder `mini_oai_server.py` (HF transformers). The
**real ROCm vLLM build emits native KV events with a schema IDENTICAL to NVIDIA vLLM 0.23.** The
incompatibility was real-vLLM vs mini-server, NOT NVIDIA vs AMD.

## Wire format observed (3-frame multipart: topic, 8-byte BE seq, msgpack payload)
Batch = [ts: float, events: [...], data_parallel_rank]. Each event is a msgspec array_like tagged
struct. Live decoded samples:

```
seq=5 topic=kv@mi350x  ts=1782454800.699  n_events=2
EVENT: ["BlockStored",
        [8738107393821479040, 5857302262393217556, 14240730501071962744],  # block_hashes
        369486939572088658,                                                 # parent_block_hash
        [10950, 17847, 13, 151645, 198, 151644, 872, 198, ...],             # token_ids (RAW)
        16,            # block_size
        null,          # lora_id
        "GPU",         # medium
        null,          # lora_name
        [null],        # extra_keys
        0,             # group_idx
        "full_attention",  # kv_cache_spec_kind
        null]          # sliding_window
EVENT: ["BlockStored", [13446450238596561779], 5857302262393217556,
        [17, 19, 20, 19, 23, 15, 15, 13, 21, 22, 18, 17, 18, 19, 151645, 198], 16,
        null, "GPU", null, [null], 0, "full_attention", null]
```

## Key facts confirmed on AMD
1. KV events publish over ZMQ PUB exactly like NVIDIA (same `BlockStored/BlockRemoved/AllBlocksCleared`,
   same field order, msgpack-encoded, 8-byte big-endian monotone seq).
2. `BlockStored.token_ids` carries the RAW prompt token sequence on AMD too -> the privacy concern
   (and our metadata-only design that DROPS token_ids at the transport boundary) applies to BOTH vendors.
3. The per-request block hashes are vLLM-internal (`ExternalBlockHash`); reproducing them in the
   sidecar to match a request is still the documented blocker -> native provider stays
   `match_supported=false` on BOTH vendors until a verified matcher exists.

## Real prefix-cache reuse observed through the sidecar
With a shared long prefix across 5 requests via the cache-aware sidecar (router-less direct):
  vllm:prefix_cache_queries_total 855 -> 1635 (+780)
  vllm:prefix_cache_hits_total    560 -> 1152 (+592)
Genuine ROCm KV prefix-cache reuse, parsed correctly by internal/runtime/vllm/metrics.go (verified:
prefix_hits supported=true val grows; kv_cache_usage_perc supported).

## Memory-profiler gotcha (ROCm)
Real vLLM's startup memory profiler intermittently computes `non_torch_increase ~241GiB` for a 0.93GiB
model under GPU contention -> "Available KV cache memory: -0.02 GiB" -> EngineCore fails. Workaround
that WORKS: bypass the profiler with `--kv-cache-memory-bytes 8589934592` (explicit 8 GiB) ->
"GPU KV cache size: 699,040 tokens", clean startup. (gpu-memory-utilization alone does not fix it.)
