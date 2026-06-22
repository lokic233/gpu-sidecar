# Collector Outage — inference unaffected

Scenario: kill the trajectory collector, then send requests through the full path.

Result (real H100):
- Non-streaming request with collector DOWN: **HTTP 200**
- Streaming request with collector DOWN: **10 SSE events received**

The trajectory emitter is non-blocking + bounded: when the collector is unreachable, events are
batched, retried with bounded backoff, then dropped (counter incremented) — the request/streaming
path never blocks. Inference continues normally. Collector is configurable by URL and its
availability is decoupled from the data path.
