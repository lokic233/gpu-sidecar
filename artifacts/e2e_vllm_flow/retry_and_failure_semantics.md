# Retry & Failure Semantics

Source: `internal/router/gateway.go`, `internal/dataplane/proxy.go`.

## Before any response bytes reach the client (RETRYABLE)
Router retries on another backend when, BEFORE the first client byte:
- connection to selected sidecar fails (sidecar_connect_failed)
- sidecar rejects (429 ADMISSION_QUEUE_FULL / 503 OFFLINE/DRAINING/UNHEALTHY)  -> sidecar_reject_NNN
- vLLM fails before the first response event (stream_err_pre_first_byte)
Rules: same logical request_id, NEW route-attempt id, failed backend recorded, wasted latency recorded,
bounded (default max_retries=1 cross-backend retry). [VALIDATED: flaky backend -> retry to h100, all 200]

## After the first response byte reaches the client (NO transparent reroute)
If the backend fails mid-stream with the client still connected:
- terminate the stream cleanly
- record PARTIAL_STREAM_FAILED with emitted events + bytes + reason
- do NOT restart generation on another backend (first implementation)

## Client cancellation
Client disconnect -> router ctx cancel -> sidecar ctx cancel + tk.cancel -> upstream vLLM request
cancelled -> vLLM stops generation. Classified CLIENT_CANCELLED/UPSTREAM_CANCELLED (NOT
PARTIAL_STREAM_FAILED). [VALIDATED: 500-token stream cut at ~1s -> propagated, 208 events then cancel]

## Distinguishing cancel from failure
ctx.Err()!=nil (client/router gone) => CANCELLED. Genuine upstream read error with client still
connected => UPSTREAM_FAILED (pre-first-byte, retryable) or PARTIAL_STREAM_FAILED (post-first-byte, terminal).
