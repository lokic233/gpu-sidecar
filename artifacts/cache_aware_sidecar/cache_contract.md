# Cache-Observation Contract

Defines the pluggable cache-observation plane: the provider interface, the prefix index semantics,
the `/v1/cache` wire format, and the privacy rules. Source of truth: `internal/cache/`.

## Provider interface (`internal/cache/provider.go`)

```go
type Provider interface {
    Mode() Mode                            // "disabled" | "explicit" | "vllm_events"
    Start(ctx context.Context) error
    Stop() error
    Snapshot() Snapshot                    // bounded metadata (hot-path safe)
    Lookup(q PrefixQuery) MatchResult      // per-request locality from local state (no I/O)
    OnRuntimeRestart()                     // invalidate locality (KV lost)
    SetKVHeadroom(headroom float64, supported bool)
    Directory(max int) map[string]int      // bounded hashed-key -> matched-tokens (router materializes)
}
```

### Modes
- **disabled** (default): `supported=false`, `Lookup` matches nothing, empty directory. Cache-aware
  policy reduces to load-only.
- **explicit**: deterministic, runtime-independent. Driven by an opaque experiment key
  (`X-Cache-Prefix-Key`) hashed before storage. Cross-vendor. Non-production / experiment-only.
- **vllm_events**: ingests native KV block-lifecycle events into the index (metadata-only).
  `match_supported=false` on this stack (documented blocker); `Lookup` returns 0/conf-0 so the
  policy falls back. Directory is empty.

## PrefixQuery / MatchResult (`model.go`)

```
PrefixQuery{ PrefixKeyHash string; PrefixTokens int }      // hashed key, claimed prefix length
MatchResult{ Supported, MatchSupported bool; MatchedPrefixTokens int; Confidence float64;
             SnapshotAgeMs int64; Reason string }
```

`MatchResult` invariants:
- `Supported=false` ⇒ provider can't observe at all (disabled). Policy must treat as "no info", not 0.
- `MatchSupported=false` ⇒ can't trust per-request matching (native blocker). 0 matched, conf 0.
- `Confidence=0` ⇒ stale/gap/unsupported. Policy ignores locality (falls back).
- `MatchedPrefixTokens` is 0 whenever confidence < floor or state is stale.

## Prefix index semantics (`index.go`)

Bounded, thread-safe, METADATA-ONLY (`IndexEntry{Key, Parent, MatchedTokens, BlockSize, Present,
LastSeq, UpdatedAt}` — **no token ids**).

| Event | Effect |
|---|---|
| store(seq,key,parent,tok,bs) | upsert present entry; evict oldest if over `MaxEntries` |
| remove(seq,key) | mark not-present (idempotent; unknown key = no-op) |
| all-clear(seq) | wipe entries, bump `resetEpoch`, count reset |
| Reset(reason) | wipe + reset seq tracking + bump epoch (runtime restart) |

Sequence handling (per publisher, monotone):
- `seq == last+1` → accept.
- `seq == last` → **duplicate**, drop (count).
- `seq < last` → **out-of-order**, apply anyway (idempotent), count.
- `seq > last+1` → **gap**, accept but `confidence` is penalized (0.25/gap, capped 0.75).

Freshness / confidence:
- `lastUpdate.IsZero()` or age > `StaleAfter` → **stale** → confidence 0, directory empty, lookups 0.
- else `confidence = (1 - age/StaleAfter) * (1 - gapPenalty)`, clamped `[0,1]`.
- per-entry `EntryTTL`: entries older than TTL are treated as absent on lookup/directory.

Bounds: `MaxEntries` hard cap (oldest-updated evicted, counted as `events_dropped`). Directory
publication is additionally capped (default 4096).

## `/v1/cache` wire format (`http_snapshot.go`, served by `internal/api/server.go`)

```json
{
  "enabled": true, "provider": "explicit", "supported": true, "match_supported": true,
  "ready": true, "confidence": 0.99, "snapshot_age_ms": 290,
  "last_event_sequence": 816, "cache_reset_epoch": 0,
  "index_entries": 15, "index_max_entries": 100000,
  "kv_headroom": 1.0, "kv_headroom_supported": true,
  "events_received_total": 816, "events_dropped_total": 0,
  "sequence_gaps_total": 0, "stale_invalidations_total": 767,
  "duplicate_events_total": 0, "out_of_order_events_total": 0, "resets_total": 0,
  "updated_at": "2026-06-25T20:..",
  "directory": { "<sha256-hex>": 400, ... }   // present + match-capable providers only; bounded
}
```

`directory` keys are SHA-256 hex of opaque experiment keys — **never** raw keys or content.

## Prometheus (`/metrics`)

16 gauges prefixed `gpu_cache_*`, labeled only `{host, provider}` — **bounded cardinality, NO
prefix hashes as labels** (task §5). E.g. `gpu_cache_confidence`, `gpu_cache_index_entries`,
`gpu_cache_sequence_gaps_total`, `gpu_cache_stale_invalidations_total`, `gpu_cache_kv_headroom`.

## Configuration flags (`cmd/sidecar`)

```
--cache-observer disabled|explicit|vllm-events   (default disabled)
--cache-event-endpoint <addr>                     (vllm-events transport; unwired on this stack)
--cache-index-max-entries <n>                     (default 100000)
--cache-entry-ttl <dur>                           (default 10m)
--cache-stale-after <dur>                          (default 30s)
--cache-explicit-header-enabled                    (default false)
```
All default to OFF — the sidecar behaves identically to before unless explicitly enabled.

## Privacy rules (hard)

1. Raw `X-Cache-Prefix-Key` is SHA-256 hashed in the proxy (`hashOpaqueKey`) before it is stored,
   emitted, or used as a directory/index key. The raw value never leaves the extract function.
2. The experimental headers are **stripped** from the request before it is forwarded to vLLM
   (even when explicit mode is disabled — hygiene).
3. Native `BlockStored.token_ids` are dropped at the transport→`BlockEvent` boundary; raw token ids
   never enter the Go process or the index.
4. No prompts/responses/token contents are logged or persisted by any cache-plane code path.
5. `/v1/cache` and `/metrics` expose only counts + hashed directory keys.
