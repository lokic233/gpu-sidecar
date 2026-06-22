# vLLM Runtime Contract

Source: `internal/runtime/model.go` (normalized), `internal/runtime/vllm/` (adapter).

## Endpoints (configurable)
base_url=http://127.0.0.1:8000, health=/health, metrics=/metrics, chat=/v1/chat/completions.
Three SEPARATE HTTP clients: metrics (500ms timeout), health (~2.5s), proxy (10m, pooled, streaming).
A slow /metrics scrape can NEVER block request streaming. Metrics scraped on a 1s loop (not per-request).

## Model / version (validated)
- vLLM 0.23.0, model Qwen/Qwen2.5-0.5B-Instruct, dtype bf16, max_model_len 4096, TP=1, --enforce-eager.
- H100: CUDA 13.0 wheel, real engine. (LD_PRELOAD bundled cublas; ninja on PATH for FlashInfer JIT.)
- MI350X: the PyPI vLLM 0.23 wheel ships ONLY CUDA C-extensions (_C.abi3.so; no _rocm_C) — it cannot
  run on ROCm without a source build. We validated the cross-vendor data plane on the REAL gfx950 GPU
  via a minimal OpenAI-compatible server (HF transformers + torch 2.10+rocm7.0) exposing vLLM-style
  /metrics. The runtime adapter parses both identically. See limitations.

## Normalized fields (mapped from real vLLM 0.23 /metrics; honest supported/unsupported)
| normalized | raw vLLM metric | H100 | MI350X(mini) |
|---|---|---|---|
| requests_running | vllm:num_requests_running | yes | yes |
| requests_waiting (RUNTIME queue) | vllm:num_requests_waiting | yes | yes |
| kv_cache_utilization | vllm:kv_cache_usage_perc | yes | yes(0) |
| generation_tokens_per_s | vllm:generation_tokens_total (counter) | yes | yes |
| ttft_p50_s | vllm:time_to_first_token_seconds (hist) | yes | unsup |
| tpot_p50_s | vllm:inter_token_latency_seconds (hist) | yes | unsup |
| request_latency_p50_s | vllm:e2e_request_latency_seconds (hist) | yes | unsup |
| requests_success_total | vllm:request_success_total | yes | yes |
| preemptions_total | vllm:num_preemptions_total | yes | unsup |
Each field carries {value, supported}; raw_metric_sources records the mapping; unsupported_fields lists gaps.
Histogram quantiles estimated from _bucket cumulative counts (documented approximation).

## RUNTIME queue vs SIDECAR queue
- requests_waiting (this contract) = vLLM internal scheduler queue.
- sidecar admission queue (queue_contract.md) = host-level admission/dispatch queue.
These are exposed SEPARATELY and never merged into one ambiguous field.
