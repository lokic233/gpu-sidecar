# Atomic Routing Snapshot Contract (P0 #5)

A routing decision reads ONE immutable snapshot: backend states AND the per-backend cache directory
share a single `Generation`, published with ONE atomic pointer swap. No decision can mix new backend/
cache metadata with an old (or newer) directory.

Source: `internal/router/registry.go` (`BackendSnapshot`, `refresh`), `internal/router/policy_cache_aware.go`.

## Shape
```go
type BackendSnapshot struct {
    Generation     uint64
    Timestamp      time.Time
    Backends       []BackendState
    CacheDirectory map[string]map[string]int // backendID -> (hashed prefix key -> READY tokens)
}
```
- `refresh()` polls all sidecars, builds states + directory, then does ONE `r.snap.Store(&snap)` with
  a monotone `gen`. There is no longer a separate `cacheDir` field / second lock (removed).
- The policy resolves locality via `snap.LookupPrefixTokens(backendID, key)` — the SAME snapshot
  pointer passed to `SelectBackend`. It never reads a mutable global directory or `g.reg`.
- The gateway captures `snap := g.reg.Snapshot()` once per request and uses it for `emitCandidateState`
  AND `SelectBackend` (one generation per attempt; a retry takes a fresh snapshot = new generation).

## Directory contents
Only READY prefixes appear (WARMING/ABSENT excluded by the residency directory). Empty for a backend
when its cache is unsupported / stale / has an unresolved sequence gap.

## Generation in trajectory
`snapshot_generation` is emitted in `BACKEND_SNAPSHOT_READ`, `CANDIDATE_STATE`, and `ROUTE_DECIDED`,
so any decision can be reconstructed and verified against one generation.

## Invariants (tested in snapshot_test.go, -race)
- `TestAtomicSnapshot_NoCrossGenerationMix`: hammering readers against rapid publishes where each
  generation is internally consistent (confidence parity == directory parity) never observes a torn
  (cross-generation) read.
- `TestAtomicSnapshot_PolicyUsesSameGeneration`: moving hotness between generations changes the
  decision accordingly — proving the policy uses the passed snapshot's directory, not a global.
- `TestAtomicSnapshot_ResetEpochAndDirectoryShareSnapshot`: reset epoch and directory always come from
  the same snapshot.
