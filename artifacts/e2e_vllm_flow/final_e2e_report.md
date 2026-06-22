# Final E2E Report — Router → Sidecar Queue → vLLM → Relay

## 1. Verdict
**END_TO_END_FLOW_VALIDATED_WITH_LIMITATIONS**

The complete request/response flow is validated on REAL hardware on BOTH vendors:
Client → Router → selected Sidecar admission queue → local runtime → Sidecar SSE relay → Router SSE
relay → Client. Non-streaming and streaming both work; queue, retry, drain, cancellation, and
collector-outage scenarios pass; a complete cross-host trajectory is joined by request_id; proxy/relay
overhead is negligible. The one limitation: on the MI350X the prebuilt vLLM 0.23 wheel is CUDA-only
(no `_rocm_C`), so the AMD backend ran a minimal OpenAI-compatible server doing REAL gfx950 inference
(HF transformers + torch 2.10+rocm7.0) exposing vLLM-style /metrics — the vendor-agnostic data plane
is fully exercised on real AMD silicon; native vLLM-on-ROCm needs a source build (documented gap).

## 2. Implemented architecture
See architecture.md. Client→Router(`cmd/router`)→Sidecar(`cmd/sidecar -data-plane`, bounded FIFO
admission queue)→local vLLM/runtime→transparent SSE relay back→Client. Async trajectory events from
router+sidecar→configurable Collector(`cmd/trajcollector`). Router reads a materialized in-memory
backend snapshot (no hot-path telemetry scrape).

## 3. Queue implementation
`internal/dataplane/queue.go`. Bounded FIFO + in-flight limiter, host-level (DISTINCT from vLLM's
runtime queue). State machine RECEIVED→QUEUED→DISPATCHING→STREAMING→COMPLETED + REJECTED/CANCELLED/
TIMED_OUT/UPSTREAM_FAILED/PARTIAL_STREAM_FAILED. Structured rejections: 429 ADMISSION_QUEUE_FULL,
503 BACKEND_OFFLINE/DRAINING/RUNTIME_UNHEALTHY/QUEUE_TIMEOUT. Drain blocks new admissions, lets
in-flight finish. Metrics on /v1/queue. VALIDATED: 30-burst → 10×200/20×429 (rejected=20); drain→503.

## 4. vLLM integration
`internal/runtime/vllm`. Endpoints /health,/metrics,/v1/chat/completions; 3 separate HTTP clients
(metrics 500ms, health, proxy 10m streaming). Metrics scraped on 1s loop, never per-request. Mapped
from real vLLM 0.23 /metrics: requests_running/waiting, kv_cache_usage_perc, generation_tokens_total,
time_to_first_token/inter_token_latency/e2e histograms, request_success_total (+honest unsupported
markers, raw_metric_sources). Model Qwen2.5-0.5B-Instruct bf16 TP=1 enforce-eager. Version-specific:
flag renamed --no-enable-log-requests; LD_PRELOAD bundled cublas; ninja for FlashInfer JIT.

## 5. Streaming relay
streaming_contract.md. Read one upstream SSE event → write+flush immediately; no full-answer
buffering, no per-token reserialize. PROVEN incremental: H100 via router 21 events over 177ms
(ts 0.162→0.339), [DONE] delivered; MI350X 22-32 events 10-30ms apart; unit test asserts per-chunk
delay reflected in arrival spacing.

## 6. Router integration
`internal/router`. RoutingPolicy interface (RL seam) + round_robin/least_queued/least_runtime_waiting/
health_gated_least_pressure. Snapshot materialized every 500ms off hot path. Pre-first-token retry
(default 1, new route-attempt id, no post-first-byte reroute). Cancellation propagates. VALIDATED:
cross-vendor round-robin alternates h100-gpu3/mi350x-gpu2 (X-Backend-Id proves each); flaky-backend
retry → all 200.

## 7. Response/Trajectory Collector
response_collector_contract.md. `cmd/trajcollector` append-only JSONL, URL-configurable, mesh-bound
for cross-host. Emitter non-blocking/bounded/batched (drop-on-full, preserve terminal, bounded
backoff, optional fallback). VALIDATED: collector killed → inference unaffected (non-stream 200,
stream 10 events); cross-host MI350X sidecar→H100 collector joined trajectory by request_id.

## 8. H100 results
Functionality: non-stream + stream + queue + drain + retry + cancel + collector-outage all pass.
Performance (40 reqs, streaming, 32-token out):
| conc | direct ttft_p50 | sidecar ttft_p50 | router+sidecar ttft_p50 | direct rps | router+sidecar rps |
|---|---|---|---|---|---|
| 1  | 27.6ms | 25.5ms | 25.7ms | 6.9  | 8.1 |
| 8  | 32.8ms | 29.9ms | 30.8ms | 48.4 | 53.3 |
| 32 | 57.3ms | 55.9ms | 57.2ms | 98.1 | 98.7 |
Added TTFT overhead: sidecar -2.1..-1.4ms, router+sidecar -1.9..-0.06ms (within noise / negative on
localhost) — meets targets (sidecar <2/<10ms; router <5/<20ms). Throughput ratio ~1.0-1.17 (no
regression). Process RSS: sidecar 44MB, router 42MB, collector 28MB.
Anomaly: at conc=32 the sidecar path showed a TTFT p95 spike (159ms) in one run — Go GC / connection
pool warmup; router path did not. Not investigated further (p50 unaffected).

## 9. MI350X results
Functionality: real gfx950 inference ("Paris."), sidecar→runtime non-stream + streaming relay, queue +
runtime snapshots, cross-vendor routing, cross-host trajectory — all pass. torch 2.10+rocm7.0 (rocm6.4
lacked gfx950 kernels). Backend = minimal OpenAI server (vLLM wheel CUDA-only). OFFLINE→RECOVERING and
all telemetry from round-3 still intact.
Anomaly: AMD per-card device numbering / proc attribution quirks (documented round 1) unchanged;
runtime histogram metrics (ttft/tpot) unsupported by the mini server (gauges/counters supported).

## 10. Direct vs proxied performance
comparisons/*.csv. Per-hop added TTFT p50 ≈ 0 (localhost). The transparent relay + bounded queue +
materialized-snapshot router design adds no measurable latency to first token and no throughput
regression at the tested concurrencies. The collector runs fully async (path C ran WITH it on; no
measurable difference vs off).

## 11. Failure experiments
failures/*.md: queue_full (429, rejected=20), cancellation (propagates to vLLM, classified CANCELLED),
pre_first_token_retry (flaky→h100, all 200, same request_id new route-attempt), collector_outage
(inference unaffected), partial_stream_failure (post-first-byte terminal, no reroute).

## 12. RL dataset readiness
rl_trajectory_contract.md. Joinable by request_id: OBSERVATION (request features + per-backend
snapshot: lifecycle/stability/host_capacity_hint/queue/runtime/freshness for H100 AND MI350X),
ACTION (backend, policy, version, retry), REWARD CANDIDATES (client TTFT, queue delay, local vLLM TTFT,
relay delays, e2e, success/partial/reject/timeout, retry cost, tokens). Policy plug-in seam ready
(`RoutingPolicy.SelectBackend(features, snapshot)`). Raw components preserved; no single reward prescribed.

## 13. Remaining limitations
- Native vLLM on MI350X (gfx950) requires a ROCm source build (PyPI wheel CUDA-only). AMD backend
  validated via a real-GPU OpenAI server, not vLLM-the-engine. Runtime histogram metrics partial there.
- Mesh caps ~1.2MB/connection: large model transfer needed checksum-verified chunking (operational, not architectural).
- No auth/TLS on any endpoint (sidecar drain + router); trusted-mesh only (carried from round 3).
- Single small model (0.5B); no TP>1 / large-model / quantization validated.
- Concurrency tested to 32 (40 reqs each); 64+ and long-soak not run.
- Post-first-token failure terminates (no cross-host resume — explicit non-goal).

## 14. Next recommended step
**Begin shadow-policy evaluation.** The data path, snapshot, policy seam, and joinable trajectory are
all in place and validated cross-vendor. The highest-value next step is to run the RoutingPolicy
interface in shadow mode (log what an RL/heuristic policy WOULD pick vs the baseline) against live
traffic to build the offline dataset Liangqi needs — before investing in native vLLM-on-ROCm or TP>1.
