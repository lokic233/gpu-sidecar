// Package cache implements the sidecar's pluggable cache-observation plane. It maintains a
// bounded, thread-safe LOCAL prefix index of cache-locality METADATA (never raw tokens) so the
// global router can make cache-aware routing decisions over a materialized, off-hot-path snapshot.
//
// Design invariants (mirror the repo's observation/data-plane split):
//   - This plane NEVER blocks streaming inference. It is observation-only.
//   - It NEVER stores or emits raw prompts, responses, token ids, or unhashed prefix/session keys.
//   - Unsupported runtime features are marked unsupported, never represented as a real zero.
//   - On any event gap, runtime restart, unsupported schema, or stale state: confidence -> 0,
//     matched_prefix_tokens -> 0, and the consumer falls back to a no-cache estimate.
//   - All cache-aware functionality defaults to DISABLED until explicitly enabled.
package cache

import "time"

// Mode selects the cache-observation provider.
type Mode string

const (
	// ModeDisabled: no cache observation. Snapshot reports supported=false; Lookup matches nothing.
	ModeDisabled Mode = "disabled"
	// ModeExplicit: deterministic experimental mode driven by opaque request prefix keys
	// (X-Cache-Prefix-Key). Works identically across vendors/runtimes because it does not depend
	// on the runtime at all. Non-production / experiment-only.
	ModeExplicit Mode = "explicit"
	// ModeVLLMEvents: native vLLM KV block-lifecycle events (BlockStored/BlockRemoved/
	// AllBlocksCleared) ingested from a transport. Metadata-only. Per-request match support depends
	// on whether a verified block-hash matcher is wired (see vllm_provider.go — NOT trusted on this
	// stack; documented blocker).
	ModeVLLMEvents Mode = "vllm_events"
)

// ParseMode maps a flag value to a Mode, defaulting to ModeDisabled for unknown values.
func ParseMode(s string) Mode {
	switch s {
	case string(ModeExplicit):
		return ModeExplicit
	case string(ModeVLLMEvents), "vllm-events":
		return ModeVLLMEvents
	case string(ModeDisabled), "":
		return ModeDisabled
	default:
		return ModeDisabled
	}
}

// PrefixQuery is the per-request input to a Lookup. It carries ONLY metadata: an already-hashed
// opaque prefix key (never the raw key) and the claimed prefix token count. No prompt content.
type PrefixQuery struct {
	// PrefixKeyHash is a hex-encoded hash of the opaque experiment prefix key (explicit mode) or a
	// hashed block-key (native mode). Empty => request is not cache-eligible.
	PrefixKeyHash string
	// PrefixTokens is the request-claimed prefix length in tokens (explicit mode) — an upper bound
	// on what could be matched. Bounded/sanitized by the caller.
	PrefixTokens int
}

// Eligible reports whether the query carries a usable prefix key.
func (q PrefixQuery) Eligible() bool { return q.PrefixKeyHash != "" }

// MatchResult is the per-request locality answer for THIS backend. It is honest about support and
// confidence so a router never treats "unknown" as a real zero.
type MatchResult struct {
	// Supported is true when this provider can observe cache locality at all on this runtime.
	Supported bool `json:"supported"`
	// MatchSupported is true when this provider can reliably answer per-request prefix matches.
	// (False for native vLLM events on this stack: request->block-hash matching is not verifiable.)
	MatchSupported bool `json:"match_supported"`
	// MatchedPrefixTokens is the number of prefix tokens believed already cached on this backend.
	// 0 whenever Supported/MatchSupported is false, confidence is low, or state is stale.
	MatchedPrefixTokens int `json:"matched_prefix_tokens"`
	// Confidence in [0,1]. 0 on gap/restart/stale/unsupported. The router must not route
	// aggressively on low confidence.
	Confidence float64 `json:"confidence"`
	// SnapshotAgeMs is the freshness of the underlying index state.
	SnapshotAgeMs int64 `json:"snapshot_age_ms"`
	// Reason is a short, label-safe explanation (no hashes/content) for trajectory/debug.
	Reason string `json:"reason"`
}

// NoMatch is the safe zero-locality answer (unsupported / disabled / stale). Confidence 0.
func NoMatch(reason string) MatchResult {
	return MatchResult{Supported: false, MatchSupported: false, MatchedPrefixTokens: 0,
		Confidence: 0, Reason: reason}
}

// Snapshot is the bounded cache-observation METADATA published via GET /v1/cache and used by the
// router registry to materialize per-backend cache state off the hot path. Bounded cardinality;
// NO prefix hashes, NO tokens, NO content.
type Snapshot struct {
	Enabled               bool      `json:"enabled"`
	Provider              string    `json:"provider"`
	Supported             bool      `json:"supported"`
	MatchSupported        bool      `json:"match_supported"`
	Ready                 bool      `json:"ready"`
	Confidence            float64   `json:"confidence"`
	SnapshotAgeMs         int64     `json:"snapshot_age_ms"`
	LastEventSequence     int64     `json:"last_event_sequence"`
	CacheResetEpoch       int64     `json:"cache_reset_epoch"`
	IndexEntries          int       `json:"index_entries"`
	IndexMaxEntries       int       `json:"index_max_entries"`
	KVHeadroom            float64   `json:"kv_headroom"` // [0,1]; 1-kv_cache_util when known
	KVHeadroomSupported   bool      `json:"kv_headroom_supported"`
	EventsReceivedTotal   uint64    `json:"events_received_total"`
	EventsDroppedTotal    uint64    `json:"events_dropped_total"`
	SequenceGapsTotal     uint64    `json:"sequence_gaps_total"`
	StaleInvalidations    uint64    `json:"stale_invalidations_total"`
	DuplicateEventsTotal  uint64    `json:"duplicate_events_total"`
	OutOfOrderEventsTotal uint64    `json:"out_of_order_events_total"`
	ResetsTotal           uint64    `json:"resets_total"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// IndexEntry is one prefix/block entry — METADATA ONLY. No raw token ids are ever stored here.
type IndexEntry struct {
	// Key is the opaque (hashed) prefix/block key.
	Key string
	// Parent is the parent block key if the protocol provides one ("" otherwise).
	Parent string
	// MatchedTokens is the token count this entry represents (block_size for native; claimed
	// prefix tokens for explicit).
	MatchedTokens int
	// BlockSize is the runtime block size in tokens (0 when not applicable, e.g. explicit mode).
	BlockSize int
	// Present is true if the block is currently stored (false after a remove).
	Present bool
	// LastSeq is the last event sequence number that touched this entry.
	LastSeq int64
	// UpdatedAt is the last update wall time (for TTL/staleness).
	UpdatedAt time.Time
}
