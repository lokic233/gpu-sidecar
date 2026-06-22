# Streaming Relay Contract

Source: `internal/dataplane/proxy.go` (sidecar), `internal/router/gateway.go` (router).

## Transparent SSE relay (NO full-answer buffering)
Both hops read one upstream SSE event (bufio line) and IMMEDIATELY write+flush it downstream.
No waiting for completion, no per-token JSON reserialize (lines forwarded byte-preserving),
no synchronous persistence before forwarding.

## Proven incremental (real hardware)
- H100 via router: 21 events spanning ~177ms (0.162s..0.339s), [DONE] delivered.
- MI350X via sidecar: ~22-32 events, ~10-30ms apart.
- Unit test TestProxy_StreamEventsIncremental: per-chunk delay reflected in arrival spacing (not all-at-once).
- [DONE] propagation verified (curl tail shows `data: [DONE]`).

## Local request context (bounded, no body retained)
request_id, route_id, backend_id, queue_entered_at, dequeued_at, upstream_connected_at,
first_upstream_event_at, first_downstream_write_at, emitted_event_count, emitted_byte_count,
completion_status. The full response body is NEVER retained.

## Backpressure / flush
bufio bounded reader (64KB); downstream write failure -> cancel upstream (CANCELLED). First event
flushed promptly; intermediate events not held; disconnect terminates the stream cleanly.

## Timing definitions (collector fields)
sidecar_queue_delay = vllm_dispatch_started - queue_entered (QUEUE_DEQUEUED.queue_wait_ms)
local_vllm_ttft     = first_vllm_event - vllm_dispatch_started (FIRST_VLLM_EVENT)
sidecar_relay_delay = first_sidecar_write - first_vllm_event (FIRST_RELAY_WRITE.relay_delay_ms)
client_observed_ttft= first_client_event - client_request_started (FIRST_CLIENT_BYTE.ttft_ms)
