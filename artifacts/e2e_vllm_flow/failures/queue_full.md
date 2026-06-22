# Queue Full — structured rejection

Sidecar config: max_queued=8, max_inflight=2. Burst of 30 concurrent long (300-token) requests.

Result (real H100):
- HTTP status: **10×200, 20×429**
- `/v1/queue` counters: `rejected_total=20, arrivals_total=10, max_queued=8, max_inflight=2`
- 429 body: `{"error":{"code":"ADMISSION_QUEUE_FULL",...}}` (structured, not silent drop)

Confirms: bounded admission queue rejects cleanly under overload; in-flight limit (2) + queue (8)
caps concurrency; requests beyond capacity get a structured 429, never dropped silently.
