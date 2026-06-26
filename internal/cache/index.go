package cache

import (
	"sort"
	"sync"
	"time"
)

// IndexConfig bounds and ages the prefix index.
type IndexConfig struct {
	MaxEntries int           // hard cap on stored entries (LRU-ish eviction by oldest update)
	EntryTTL   time.Duration // entries older than this are treated as absent on lookup/snapshot
	StaleAfter time.Duration // if no event for this long, the whole index is considered stale (conf=0)
	BlockSize  int           // default block size in tokens (native); informational for explicit
}

// DefaultIndexConfig returns conservative bounds.
func DefaultIndexConfig() IndexConfig {
	return IndexConfig{
		MaxEntries: 100_000,
		EntryTTL:   10 * time.Minute,
		StaleAfter: 30 * time.Second,
		BlockSize:  16,
	}
}

// Index is a bounded, thread-safe prefix/block METADATA store. It is the shared core used by every
// provider. It tracks event sequencing (gap detection), a reset epoch (runtime restart / all-clear),
// staleness, and bounded eviction. It NEVER stores raw token ids or content.
//
// Concurrency: a single sync.RWMutex guards all state. Writes (events) take the write lock; reads
// (Lookup/Snapshot) take the read lock. The hot path never calls into the Index synchronously for
// routing — the router reads a materialized Snapshot — but Lookup is cheap and lock-bounded.
type Index struct {
	cfg IndexConfig
	now func() time.Time // injectable clock for tests

	mu      sync.RWMutex
	entries map[string]*IndexEntry

	// sequencing / freshness
	lastSeq    int64
	haveSeq    bool
	resetEpoch int64
	lastUpdate time.Time
	startedAt  time.Time

	// trust state (current correctness condition, NOT a cumulative penalty)
	unresolvedGap bool // true after a detected sequence gap; cleared only by Reset/rebuild
	wasStale      bool // tracks fresh<->stale transitions so the stale counter increments once

	// counters (monotonic; for /v1/cache + metrics)
	eventsReceived   uint64
	eventsDropped    uint64
	sequenceGaps     uint64
	duplicates       uint64
	outOfOrder       uint64
	staleInvalidated uint64
	resets           uint64
}

// NewIndex builds an empty index.
func NewIndex(cfg IndexConfig) *Index {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultIndexConfig().MaxEntries
	}
	now := time.Now
	return &Index{
		cfg: cfg, now: now, entries: make(map[string]*IndexEntry),
		startedAt: now(), lastUpdate: time.Time{},
	}
}

// WithClock overrides the clock (tests only).
func (idx *Index) WithClock(f func() time.Time) *Index {
	idx.mu.Lock()
	idx.now = f
	idx.startedAt = f()
	idx.mu.Unlock()
	return idx
}

// --- event application -------------------------------------------------------------------------

// checkSeqLocked validates a BATCH sequence number against the last seen one. Returns:
//
//	accept=true   -> apply this event
//	accept=false  -> drop (an older batch we have already fully consumed)
//
// IMPORTANT: vLLM's KV-event sequence is per-BATCH, not per-event. A single KVEventBatch (one seq)
// carries MANY BlockStored/BlockRemoved events, so multiple events legitimately share one seq. We
// therefore treat `seq == lastSeq` as a same-batch continuation (accept), detect gaps only when the
// seq jumps forward by >1 batch, and count true duplicates separately at the entry level (a store
// for a key+seq we already recorded). Caller holds the write lock.
func (idx *Index) checkSeqLocked(seq int64) (accept bool) {
	if !idx.haveSeq {
		idx.haveSeq = true
		idx.lastSeq = seq
		return true
	}
	switch {
	case seq == idx.lastSeq+1:
		idx.lastSeq = seq
		return true
	case seq == idx.lastSeq:
		// same batch as the most recent one — additional events from that batch. Accept.
		return true
	case seq < idx.lastSeq:
		// an older batch arriving late / replayed. Accept at the batch level, but PER-ENTRY ordering
		// (enforced in ApplyStore/ApplyRemove) prevents an old event from overriding newer state.
		idx.outOfOrder++
		return true
	default: // seq > lastSeq+1 : we skipped at least one whole batch -> GAP (correctness condition).
		idx.sequenceGaps++
		idx.lastSeq = seq
		// A gap means the directory may have missed a store OR a removal. We can no longer trust
		// completeness: set the unresolved-gap trust flag (confidence -> 0, no matchable directory)
		// until a verified Reset/rebuild restores trust. This is NOT a cumulative soft penalty.
		idx.unresolvedGap = true
		return true
	}
}

// ApplyStore records a block/prefix store event. key is the opaque (hashed) block key; parent is the
// parent block key ("" if none); matchedTokens is the token count this block represents; blockSize is
// the runtime block size; seq is the event sequence number (monotone per publisher).
func (idx *Index) ApplyStore(seq int64, key, parent string, matchedTokens, blockSize int) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.eventsReceived++
	if !idx.checkSeqLocked(seq) {
		return
	}
	t := idx.now()
	idx.lastUpdate = t
	if e, ok := idx.entries[key]; ok {
		// A store for a key we already have AT THE SAME SEQ is a true duplicate (e.g. replayed batch
		// or at-least-once redelivery). Count it and no-op — the entry is unchanged.
		if e.LastSeq == seq && e.Present {
			idx.duplicates++
			return
		}
		// PER-ENTRY ORDERING: an OLD store (seq < this entry's last seq) must not resurrect a newer
		// removal or clobber newer state. Drop it (counted at batch level as out-of-order).
		if seq < e.LastSeq {
			return
		}
		e.Present = true
		e.Parent = parent
		if matchedTokens > 0 {
			e.MatchedTokens = matchedTokens
		}
		if blockSize > 0 {
			e.BlockSize = blockSize
		}
		e.LastSeq = seq
		e.UpdatedAt = t
		return
	}
	idx.entries[key] = &IndexEntry{
		Key: key, Parent: parent, MatchedTokens: matchedTokens, BlockSize: blockSize,
		Present: true, LastSeq: seq, UpdatedAt: t,
	}
	idx.evictIfNeededLocked()
}

// ApplyRemove records a block removal. The entry is marked not-present (kept briefly so a duplicate
// remove is a no-op and so we can answer "was present" honestly), then eligible for eviction.
func (idx *Index) ApplyRemove(seq int64, key string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.eventsReceived++
	if !idx.checkSeqLocked(seq) {
		return
	}
	t := idx.now()
	idx.lastUpdate = t
	if e, ok := idx.entries[key]; ok {
		// PER-ENTRY ORDERING: an OLD remove (seq < this entry's last seq) must not delete a NEWER
		// store. Drop it. An equal-seq remove for an already-removed entry is an idempotent duplicate.
		if seq < e.LastSeq {
			return
		}
		if seq == e.LastSeq && !e.Present {
			idx.duplicates++
			return
		}
		e.Present = false
		e.LastSeq = seq
		e.UpdatedAt = t
	}
	// removing a key we never saw is a no-op (idempotent)
}

// ApplyClear handles AllBlocksCleared: drop all locality. Bumps the reset epoch.
func (idx *Index) ApplyClear(seq int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.eventsReceived++
	_ = idx.checkSeqLocked(seq) // advances/repairs seq tracking; clear applies regardless
	idx.entries = make(map[string]*IndexEntry)
	idx.resetEpoch++
	idx.resets++
	// AllBlocksCleared is a verified rebuild boundary: the directory is now consistent again, so the
	// unresolved-gap trust flag is cleared (trust restored after a verified reset).
	idx.unresolvedGap = false
	idx.lastUpdate = idx.now()
}

// Reset wipes all state and bumps the reset epoch. Used on detected runtime restart / provider
// (re)start. Sequence tracking is reset so the next event re-seeds lastSeq without a false gap.
func (idx *Index) Reset(reason string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.entries = make(map[string]*IndexEntry)
	idx.haveSeq = false
	idx.lastSeq = 0
	idx.resetEpoch++
	idx.resets++
	// a verified reset/rebuild restores sequence trust.
	idx.unresolvedGap = false
	idx.wasStale = false
	idx.lastUpdate = time.Time{}
	idx.startedAt = idx.now()
}

// evictIfNeededLocked enforces MaxEntries by removing the oldest-updated entries. Caller holds wlock.
func (idx *Index) evictIfNeededLocked() {
	over := len(idx.entries) - idx.cfg.MaxEntries
	if over <= 0 {
		return
	}
	// find `over` oldest entries by UpdatedAt. MaxEntries is large and evictions rare, so a simple
	// scan is acceptable and avoids a second heap structure.
	for over > 0 {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range idx.entries {
			if first || e.UpdatedAt.Before(oldest) {
				oldest = e.UpdatedAt
				oldestKey = k
				first = false
			}
		}
		if oldestKey == "" {
			return
		}
		delete(idx.entries, oldestKey)
		idx.eventsDropped++ // an evicted entry is locality we can no longer answer for
		over--
	}
}

// --- reads -------------------------------------------------------------------------------------

// isStaleLocked reports whether the index is too old to trust. Caller holds at least the read lock.
func (idx *Index) isStaleLocked() bool {
	if idx.cfg.StaleAfter <= 0 {
		return false
	}
	if idx.lastUpdate.IsZero() {
		return true // never received an event
	}
	return idx.now().Sub(idx.lastUpdate) > idx.cfg.StaleAfter
}

// confidenceLocked computes a [0,1] confidence from freshness and recent gap pressure. Caller holds
// at least the read lock. Gaps reduce confidence; staleness zeroes it.
func (idx *Index) confidenceLocked() float64 {
	if idx.lastUpdate.IsZero() || idx.isStaleLocked() {
		return 0
	}
	// Unresolved sequence gap => we cannot trust completeness. Confidence is 0 (and no matchable
	// directory is published) until a verified Reset/rebuild restores trust. NOT a soft cumulative
	// penalty that lingers forever.
	if idx.unresolvedGap {
		return 0
	}
	age := idx.now().Sub(idx.lastUpdate)
	conf := 1.0
	// linear freshness decay across the stale window
	if idx.cfg.StaleAfter > 0 {
		conf = 1.0 - age.Seconds()/idx.cfg.StaleAfter.Seconds()
		if conf < 0 {
			conf = 0
		}
	}
	if conf < 0 {
		conf = 0
	}
	return conf
}

// sequenceHealthyLocked reports whether the directory is currently trustworthy (no unresolved gap).
func (idx *Index) sequenceHealthyLocked() bool { return !idx.unresolvedGap }

// lookupKeyLocked returns the matched-token total for a key if present & not TTL-expired. Caller
// holds the read lock.
func (idx *Index) lookupKeyLocked(key string) (tokens int, present bool) {
	e, ok := idx.entries[key]
	if !ok || !e.Present {
		return 0, false
	}
	if idx.cfg.EntryTTL > 0 && idx.now().Sub(e.UpdatedAt) > idx.cfg.EntryTTL {
		return 0, false // expired
	}
	return e.MatchedTokens, true
}

// LookupKey answers whether a single opaque key is currently cached, and for how many tokens.
// Returns confidence-adjusted result (0 tokens when stale). This is the primitive the explicit
// provider uses.
func (idx *Index) LookupKey(key string) (tokens int, confidence float64, ageMs int64) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.isStaleLocked() {
		return 0, 0, idx.ageMsLocked()
	}
	conf := idx.confidenceLocked()
	t, present := idx.lookupKeyLocked(key)
	if !present {
		return 0, conf, idx.ageMsLocked()
	}
	return t, conf, idx.ageMsLocked()
}

func (idx *Index) ageMsLocked() int64 {
	if idx.lastUpdate.IsZero() {
		return -1
	}
	return idx.now().Sub(idx.lastUpdate).Milliseconds()
}

// SnapshotMeta returns bounded metadata about the index for /v1/cache and metrics. It also performs
// stale invalidation accounting (write path) when the index has gone stale since the last snapshot.
func (idx *Index) SnapshotMeta() Snapshot {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	stale := idx.isStaleLocked()
	// Increment the stale-invalidation counter ONCE per fresh->stale transition (not on every poll
	// while already stale). Track the current stale state explicitly.
	if stale && !idx.wasStale && !idx.lastUpdate.IsZero() {
		idx.staleInvalidated++
	}
	idx.wasStale = stale
	conf := idx.confidenceLocked()
	present := 0
	for _, e := range idx.entries {
		if e.Present {
			present++
		}
	}
	age := int64(-1)
	if !idx.lastUpdate.IsZero() {
		age = idx.now().Sub(idx.lastUpdate).Milliseconds()
	}
	return Snapshot{
		Confidence:            conf,
		SnapshotAgeMs:         age,
		LastEventSequence:     idx.lastSeq,
		CacheResetEpoch:       idx.resetEpoch,
		IndexEntries:          present,
		IndexMaxEntries:       idx.cfg.MaxEntries,
		EventsReceivedTotal:   idx.eventsReceived,
		EventsDroppedTotal:    idx.eventsDropped,
		SequenceGapsTotal:     idx.sequenceGaps,
		StaleInvalidations:    idx.staleInvalidated,
		DuplicateEventsTotal:  idx.duplicates,
		OutOfOrderEventsTotal: idx.outOfOrder,
		ResetsTotal:           idx.resets,
		SequenceHealthy:       idx.sequenceHealthyLocked(),
		UnresolvedGap:         idx.unresolvedGap,
		Ready:                 !stale && !idx.unresolvedGap && !idx.lastUpdate.IsZero(),
		UpdatedAt:             idx.lastUpdate,
	}
}

// Len returns the number of entries (present or tombstoned) — for tests/metrics.
func (idx *Index) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}

// ResetEpoch returns the current reset epoch.
func (idx *Index) ResetEpoch() int64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.resetEpoch
}

// Directory returns a bounded snapshot of present, fresh prefix keys -> matched token counts. It is
// the data the router materializes (off the hot path) so a routing decision can match the current
// request's prefix key with an O(1) LOCAL map lookup — never a per-request network query. Keys are
// already opaque (hashed); no content is exposed. Capped at `max` entries (most-recently-updated).
// Returns an empty (non-nil) map when stale or empty.
func (idx *Index) Directory(max int) map[string]int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make(map[string]int)
	if idx.isStaleLocked() || idx.unresolvedGap || idx.lastUpdate.IsZero() {
		return out // stale OR unresolved gap => publish nothing (router falls back to no-cache)
	}
	if max <= 0 {
		max = idx.cfg.MaxEntries
	}
	ttl := idx.cfg.EntryTTL
	now := idx.now()
	// If under the cap, just emit all present+fresh entries.
	if len(idx.entries) <= max {
		for k, e := range idx.entries {
			if e.Present && (ttl <= 0 || now.Sub(e.UpdatedAt) <= ttl) {
				out[k] = e.MatchedTokens
			}
		}
		return out
	}
	// Over the cap: select the `max` most-recently-updated present+fresh entries. Bounded scan +
	// sort by recency (the directory cap is small relative to MaxEntries in practice).
	cand := make([]dirEntry, 0, len(idx.entries))
	for k, e := range idx.entries {
		if e.Present && (ttl <= 0 || now.Sub(e.UpdatedAt) <= ttl) {
			cand = append(cand, dirEntry{k, e.UpdatedAt, e.MatchedTokens})
		}
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].t.After(cand[j].t) })
	if len(cand) > max {
		cand = cand[:max]
	}
	for _, c := range cand {
		out[c.key] = c.tok
	}
	return out
}

// dirEntry is a transient row used to select the most-recent directory entries under the cap.
type dirEntry struct {
	key string
	t   time.Time
	tok int
}
