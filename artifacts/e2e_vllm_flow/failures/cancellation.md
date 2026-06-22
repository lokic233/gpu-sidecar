# Client Cancellation — propagation to vLLM

Scenario: streaming request (max_tokens=500), client disconnects after ~1s.

Result (real H100, joined trajectory by request_id=canceltest1):
```
router   FIRST_CLIENT_BYTE      ttft_ms=31.2
... 208 SSE events streamed ...
router   CLIENT_CANCELLED / UPSTREAM_CANCELLED (mid_stream)
sidecar  UPSTREAM_CANCELLED (mid_stream)  -> vLLM request context cancelled
sidecar  STATE_TRANSITION STREAMING -> CANCELLED
```
Propagation chain: Client disconnect → Router cancels sidecar request (ctx) → Sidecar cancels the
upstream vLLM HTTP request (tk.cancel + ctx) → vLLM stops generation. Cancellation classified as
CANCELLED (not PARTIAL_STREAM_FAILED) after the fix. Abandoned generation does not continue.
