# Service Profile Contract (P1 #6)

Decouples a backend's SLOW/STATIC service capability (BackendProfile) from its LIVE congestion
(LiveBackendState). The cache-aware policy uses the PROFILE for decode/prefill cost and LIVE state
only for the queue/congestion term — so a busy backend never becomes "faster".

Source: `internal/router/policy_cache_aware.go` (`BackendProfile`, `decodeMsPerToken`), `cmd/router`
(`--profiles`), `internal/router/registry.go` (aggregate throughput kept as TELEMETRY).

## The bug this fixes
Previously decode cost = `1000 / GenTokensPerSec` where `GenTokensPerSec = Δgeneration_tokens_total/Δt`
(aggregate). Under continuous batching a BUSIER backend reports HIGHER aggregate throughput → policy
thinks it is faster → sends more → busier. A herding/starvation positive-feedback loop.

## The split
- **BackendProfile** (static, configured/calibrated offline): `decode_ms_per_token`,
  `prefill_ms_per_token`, `version`, `confidence`. Supplied via `--profiles '{"id":{...}}'`.
- **Global fallback**: `FallbackDecodeMsPerToken` (default 0.30 ms/tok). Used when a backend has NO
  profile; recorded per candidate as `profile_fallback=true` in `CandidateScore`/`CANDIDATE_STATE`.
- **LiveBackendState** (drives congestion only): `QueueDepth`, `QueueInflight`, `RuntimeWaiting`,
  `RuntimeRunning`, `KVHeadroom`, stability/health. A growing backlog RAISES the queue term, making a
  busy backend LESS attractive — the opposite of the old loop.
- **Aggregate throughput** (`GenTokensPerSec`, `ServiceRateSupported`) is **preserved as telemetry**
  in `BackendState`/`CANDIDATE_STATE` but is NEVER used as per-request decode speed by the default
  analytical policy.

## Invariants (tested)
- `TestCacheAware_HeterogeneousProfilesRespected`: faster-profile backend wins (capability respected).
- `TestCacheAware_BusyAggregateThroughputDoesNotAttract`: a backend with HIGH live aggregate
  throughput AND a growing backlog LOSES to an idle one; and its analytical decode cost is identical
  to the idle backend's (proving aggregate throughput is not used as speed).
- `TestCacheAware_ProfileFallbackRecorded`: fallback usage is flagged.

## Scope (this task)
Minimal acceptable implementation = per-backend profile in router config + explicit fallback. NO
online profiler is built. A future online calibrator can populate profiles without changing the
policy.
