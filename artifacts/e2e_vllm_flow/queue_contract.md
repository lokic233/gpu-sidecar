# Sidecar Admission Queue Contract

Source: `internal/dataplane/queue.go`. HOST-level admission/dispatch queue, DISTINCT from vLLM's
runtime scheduler queue (vllm:num_requests_waiting).

## Config (validated)
max_queued_requests, max_inflight_requests, queue_timeout, admission_mode=fifo.

## Request state machine
RECEIVED -> QUEUED -> DISPATCHING -> STREAMING|WAITING_FOR_FULL_RESPONSE -> COMPLETED
Failure: REJECTED, CANCELLED, TIMED_OUT, UPSTREAM_FAILED, PARTIAL_STREAM_FAILED.
Every transition event carries request_id, route_id, backend_id, host_id, device_id, wall time,
monotonic duration, from, to, reason.

## Admission gate (rejects cleanly, structured errors — never silent drop)
- BACKEND_OFFLINE (lifecycle OFFLINE)       -> 503
- BACKEND_DRAINING (operator drain)         -> 503   [validated: drain -> 503, undrain -> 200]
- RUNTIME_UNHEALTHY (vLLM /health fails)    -> 503
- ADMISSION_QUEUE_FULL                       -> 429   [validated: 30 burst -> 10x200/20x429, rejected=20]
- QUEUE_TIMEOUT                              -> 503

## Exposed metrics (/v1/queue)
queued_requests, inflight_requests, max_queued_requests, max_inflight_requests, oldest_queued_age_ms,
arrival_rate_per_s, dispatch_rate_per_s, completion_rate_per_s, arrivals/dispatched/completed/rejected/
queue_timeout/cancelled totals, queue_wait_p50_ms, queue_wait_p95_ms.

## Drain
Operator drains via POST /v1/drain {device,on}. While draining, new admissions get 503 BACKEND_DRAINING;
in-flight requests continue (the gate only blocks NEW admissions). [validated on H100]
