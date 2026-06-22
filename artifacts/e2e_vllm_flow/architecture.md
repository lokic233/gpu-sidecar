# E2E vLLM Flow — Implemented Architecture

## Deployed request/response path (VALIDATED on real H100 + MI350X)
```diagram
Client (OpenAI-compatible HTTP)
  │  POST /v1/chat/completions  (stream=true|false)
  ▼
Global Router Gateway        cmd/router, internal/router
  │  reads in-memory BACKEND SNAPSHOT (materialized off hot path; NO per-request telemetry scrape)
  │  policy.SelectBackend(features, snapshot)  -> H100 or MI350X
  │  bounded pre-first-token retry (default 1)
  ▼
Host-local GPU Sidecar       cmd/sidecar -data-plane, internal/dataplane
  │  Admit -> bounded FIFO admission queue (host-level)  [DISTINCT from vLLM runtime queue]
  │  dispatch (in-flight limiter) -> own upstream vLLM connection
  ▼
Local vLLM OpenAI Server     (H100: real vLLM 0.23 / MI350X: real gfx950 OpenAI runtime)
  │  JSON or SSE chunks
  ▲
Sidecar transparent relay    (read event -> write+flush immediately; NO full-answer buffering)
  ▲
Router transparent relay
  ▲
Client  (incremental SSE; [DONE]; cancellation propagates downstream->upstream->vLLM)

           ┌── async, non-blocking, bounded, batched ──┐
Router ────┤                                            ├──> Response/Trajectory Collector
Sidecar ───┘   (configurable URL; on router host now)        cmd/trajcollector  (NOT a proxy)
```

## Two sidecar planes (logically isolated)
- OBSERVATION plane (existing, preserved): GPU telemetry, lifecycle, stability/recovery, vLLM runtime
  metrics, queue metrics, readiness, backend snapshot publication. Slow nvidia-smi/rocm-smi/metrics
  scrape on its own loop — NEVER on the request path.
- DATA plane (new): admit -> bounded queue -> dispatch -> own vLLM connection -> relay -> cancel-propagate
  -> emit async local events. Continues operating during transient telemetry failure (separate clients).

## Invariants honored
- Router owns client connection, global selection, request/route IDs, pre-first-token retry, client SSE,
  cancellation, async trajectory emission; reads a materialized snapshot (no hot-path telemetry poll).
- Sidecar owns local admission/dispatch/lifecycle, upstream vLLM connection, relay, cancellation, local timing.
- vLLM owns model serving, scheduling, KV-cache, prefill/decode, token gen, OpenAI formatting, SSE.
- Collector is async metadata persistence only; its failure cannot block the data path.

## Components / code paths
- internal/runtime + internal/runtime/vllm — runtime adapter (3 separate HTTP clients; prom parser)
- internal/dataplane — queue.go (admission), proxy.go (OpenAI proxy + SSE relay)
- internal/trajectory — emitter.go (async bounded batched), sink.go
- internal/router — registry.go (snapshot), policy.go (RL seam), gateway.go (relay/retry/cancel)
- cmd/{sidecar -data-plane, router, trajcollector}
