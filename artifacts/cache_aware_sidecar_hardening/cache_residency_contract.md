# Cache Residency Contract (P0 #3)

Replaces the old binary `Observe()` (which marked a prefix cached at DISPATCH — too early) with a
conservative per-prefix residency STATE MACHINE. A prefix is a reusable cache hit ONLY in READY.

Source: `internal/cache/residency.go`, `internal/cache/explicit_provider.go`,
`internal/dataplane/proxy.go` (`resolveTerminal`).

## States & transitions
```
ABSENT ──BeginWarm──▶ WARMING ──MarkReady──▶ READY
   ▲                     │                      │
   └──AbortWarm(last)────┘                      │
   ◀──────Reset / AllBlocksCleared / TTL────────┘
```

| Transition | Trigger (proxy) |
|---|---|
| ABSENT → WARMING | a cache-eligible request is successfully dispatched to the local runtime (`BeginWarm`) |
| WARMING → READY | streaming: FIRST valid upstream model event (first token); non-streaming: successful 2xx (`MarkReady`) |
| WARMING → ABSENT | pre-first-token failure, non-2xx, cancel-before-readiness, when NO other request is still warming (`AbortWarm`) |
| READY/WARMING → ABSENT | runtime restart, AllBlocksCleared, TTL expiry (`Reset`) |

## Invariants (tested in residency_test.go / cache_proxy_test.go)
- A WARMING prefix is **never** a reusable hit: `Lookup` returns 0 reusable tokens and it is **excluded
  from the routable directory**. (Router cannot route to a not-yet-cached prefix.)
- Concurrent requests warming the same key are refcounted: AbortWarm only returns the key to ABSENT
  when the LAST warmer fails and none readied. A late AbortWarm after a concurrent MarkReady does NOT
  un-ready the key.
- MarkReady on an ABSENT key (e.g. a stale readiness signal after reset/abort) does NOT resurrect it
  — counted as `false_ready` and ignored. (A stale readiness cannot revive evicted locality.)
- Duplicate terminal events are idempotent (double MarkReady = no-op; double AbortWarm = no-op).
- Runtime restart / AllBlocksCleared wipes WARMING and READY and bumps the reset epoch.
- TTL expiry lazily demotes a READY entry to ABSENT (and is swept by Stats()).

## Native-event mode
For `vllm_events`, the preferred READY signal is verified `BlockStored` evidence. On THIS stack
native per-request matching is unsupported (see remaining_blockers.md), so the native provider stays
metadata-only and does not drive request-level residency.

## Honesty
Explicit mode is labeled NON-PRODUCTION / experiment-only. It does NOT claim to reflect native vLLM
block eviction exactly — it is a conservative residency model that is correct about lifecycle
(warming ≠ ready) and safe under failure (abort/restart/stale → not a hit).

## Counters (in /v1/cache)
`residency_ready`, `residency_warming`, `residency_total`, `false_ready_total`, `resets_total`,
`cache_reset_epoch`.
