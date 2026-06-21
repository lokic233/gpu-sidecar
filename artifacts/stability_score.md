# Stability Score

Source: `internal/core/stability.go`. Tested in `stability_test.go`.
An **operational** signal in [0,1] — explicitly NOT a universal scientific metric. Fully transparent:
the formula is a weighted sum of bounded sub-scores, and every component is exposed in
`/v1/status → devices[].stability.components` so a consumer can audit it.

## Instantaneous score
```
inst = Wavail·avail
     + Wfail ·exp(-0.7·consecutive_failures)·exp(-0.5·process_crashes)
     + Wdisc ·exp(-0.5·disconnects_in_window)
     + Wrec  ·exp(-recovery_ms/30000)
     + Werr  ·exp(-1.5·new_uncorrectable_errors)
     + Wlat  ·clamp(1 - (p95/p50 - 1)/9)
     + Wthr  ·clamp(1 - 2·throughput_cov)      # neutral 1.0 when throughput probe disabled
```
Weights (sum=1.0): availability 0.30, failures 0.20, disconnect 0.15, recovery 0.10,
errors 0.10, latency 0.10, throughput 0.05. All sub-scores are clamped to [0,1]; NaN/Inf → 0.

## Memory of instability (asymmetric EWMA)
The reported score is a smoothed value, not the instantaneous one:
```
if inst < smoothed:  smoothed = AlphaUp·inst   + (1-AlphaUp)·smoothed     # AlphaUp=0.60  → drops fast
else:                smoothed = AlphaDown·inst + (1-AlphaDown)·smoothed   # AlphaDown=0.08 → recovers slow
```
Consequences (verified by tests + live experiments):
- Instability drops the score **quickly** (high alpha on the down direction).
- **One good probe cannot erase recent instability** (`TestStabilityDecayFastRecoverSlow`).
- Recovery requires *sustained* health: with AlphaDown=0.08, climbing from a deep dip back to
  ~0.95 takes on the order of dozens of healthy samples (tunable). See the recovery trajectory
  in `artifacts/time_series/stability_recovery_*.json`.

## Auditing
`components` includes each weighted contribution plus `instantaneous` and `smoothed`, so an
external router can see *why* a score is what it is rather than trusting a black box.
