# Response/Trajectory Collector Contract

Source: `internal/trajectory/` (emitter), `cmd/trajcollector/` (collector).

## NOT a response proxy
The collector asynchronously RECEIVES and PERSISTS metadata/outcome events. It never sits on the
request/response data path. Its failure or slowness cannot block routing/queue/inference/streaming.

## Configurable by URL (deployment-flexible)
collector_url configured on BOTH router and sidecars. For this experiment the collector runs on the
router host ([::]:29100); the MI350X sidecar ships events to it over the IPv6 mesh (VALIDATED:
cross-host joined trajectory for a request routed to MI350X). It can later run on any reachable host
with no router/sidecar code change — only the URL.

## Non-blocking delivery (VALIDATED)
Emitter: bounded queue (default 10000), batched (128) + interval (500ms) flush, 500ms timeout, one
bounded-backoff retry, optional bounded JSONL fallback. Queue-full -> drop (counter++), preserving
terminal events with a tiny attempt. Collector outage -> requests + streaming continue unaffected
(killed collector: non-stream 200, stream 10 events delivered).

## Stored schema (privacy: no prompts/responses)
kind, source, request_id, route_id, backend_id, host_id, device_id, wall, fields{} (counts, length
estimates, timing, backend state, route action, outcome, error class). Content logging OFF by default.

## Event types
Router: REQUEST_RECEIVED, BACKEND_SNAPSHOT_READ, ROUTE_DECIDED, ROUTE_ATTEMPT_STARTED,
ROUTE_ATTEMPT_FAILED, FIRST_CLIENT_BYTE, CLIENT_CANCELLED, REQUEST_COMPLETED, PARTIAL_STREAM_FAILED.
Sidecar: LOCAL_REQUEST_RECEIVED, QUEUE_ENTERED, QUEUE_DEQUEUED, VLLM_DISPATCH_STARTED, VLLM_CONNECTED,
FIRST_VLLM_EVENT, FIRST_RELAY_WRITE, STREAM_COMPLETED, UPSTREAM_CANCELLED, QUEUE_REJECTED,
QUEUE_TIMED_OUT, VLLM_REQUEST_FAILED, STATE_TRANSITION.
