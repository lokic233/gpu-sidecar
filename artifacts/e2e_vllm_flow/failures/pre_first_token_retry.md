# Pre-First-Token Retry

Scenario: router with a "flaky" backend (healthy in snapshot, 503s on /v1/chat/completions) listed
first + real h100-gpu3, round_robin policy, max_retries=1.

Result (real H100, joined trajectory, requests flakytest1..6):
```
ROUTE_DECIDED        backend=flaky       (rr)
ROUTE_ATTEMPT_FAILED backend=flaky       reason=sidecar_reject_503
ROUTE_DECIDED        backend=h100-gpu3   (rr, retry to alternative)
FIRST_CLIENT_BYTE    backend=h100-gpu3
REQUEST_COMPLETED    backend=h100-gpu3
```
All 6 requests returned HTTP 200 to the client. Same logical request_id, new route-attempt id per
attempt, failed backend recorded, wasted latency recorded. Retry happens ONLY before the first
client byte; default at most one cross-backend retry.
