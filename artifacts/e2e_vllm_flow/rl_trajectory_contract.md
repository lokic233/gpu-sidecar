# RL Trajectory Contract

A joinable trajectory record is assembled by `request_id` across router + sidecar events (collector
JSONL). It separates OBSERVATION (route-time state), ACTION (the decision), and REWARD CANDIDATES
(measured outcome components) so multiple reward formulations can be tested. No single reward is prescribed.

## Join key
`request_id` (stable across attempts) + `route_id` (per attempt). Events carry backend_id/host_id/device_id.

## Observation (route-time, from BACKEND_SNAPSHOT_READ + request features)
- request: input_len_est, requested_output (max_tokens), stream, model, slo_class
- per-backend snapshot (immutable, materialized off hot path) for H100 AND MI350X:
  - lifecycle_state, control_plane_ready, stability_score, host_capacity_hint
  - sidecar admission queue: queue_depth, queue_inflight, queue_max  (HOST-level)
  - vLLM runtime: runtime_waiting, runtime_running, kv_cache_util     (RUNTIME-level, DISTINCT)
  - snapshot_age_ms (state freshness)

## Action (ROUTE_DECIDED)
- selected backend_id, policy name, policy version, reason
- retry action (route_attempt index; failed backend recorded on ROUTE_ATTEMPT_FAILED)

## Reward candidates (measured; raw components preserved)
- client_observed_ttft_ms       (FIRST_CLIENT_BYTE - REQUEST_RECEIVED)
- sidecar_queue_delay_ms        (QUEUE_DEQUEUED.queue_wait_ms)
- local_vllm_ttft_ms            (FIRST_VLLM_EVENT.vllm_ttft_ms_from_dispatch)
- sidecar_first_event_relay_ms  (FIRST_RELAY_WRITE.relay_delay_ms)
- e2e_latency_ms                (REQUEST_COMPLETED.e2e_ms)
- completed (success), partial_stream_failed, rejected (QUEUE_REJECTED), queue_timeout
- retry_cost (wasted_ms on ROUTE_ATTEMPT_FAILED)
- output tokens (from usage when present) -> tokens/s derivable
- slo_violation (consumer-defined threshold vs ttft/e2e)
- estimated compute/energy cost: derivable from backend host telemetry (power_watts) at route time

## Event types (collector)
Router: REQUEST_RECEIVED, BACKEND_SNAPSHOT_READ, ROUTE_DECIDED, ROUTE_ATTEMPT_STARTED,
  ROUTE_ATTEMPT_FAILED, FIRST_CLIENT_BYTE, CLIENT_CANCELLED, REQUEST_COMPLETED, PARTIAL_STREAM_FAILED
Sidecar: LOCAL_REQUEST_RECEIVED, QUEUE_ENTERED, QUEUE_DEQUEUED, VLLM_DISPATCH_STARTED, VLLM_CONNECTED,
  FIRST_VLLM_EVENT, FIRST_RELAY_WRITE, STREAM_COMPLETED, UPSTREAM_CANCELLED, QUEUE_REJECTED,
  QUEUE_TIMED_OUT, VLLM_REQUEST_FAILED, + STATE_TRANSITION (RECEIVED..COMPLETED + failure states)

## Policy plug-in seam (for Liangqi's RL policy)
`internal/router/policy.go`:
    type RoutingPolicy interface {
        SelectBackend(request RequestFeatures, snapshot *BackendSnapshot) (RouteDecision, error)
    }
Baseline policies: round_robin, least_queued, least_runtime_waiting, health_gated_least_pressure.
An RL policy implements the same interface, reading the materialized snapshot (pure, no I/O).

## Privacy
No prompts/responses stored. Only counts, length estimates, model name, timing, backend state,
route action, outcome, error class. (Content logging OFF by default.)
