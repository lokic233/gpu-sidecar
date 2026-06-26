package cache

import (
	"sync"
	"time"
)

// ResidencyState is the per-prefix lifecycle. A prefix is only a REUSABLE cache hit in READY.
type ResidencyState string

const (
	StateAbsent  ResidencyState = "ABSENT"
	StateWarming ResidencyState = "WARMING"
	StateReady   ResidencyState = "READY"
)

// residencyEntry is the per-prefix-key residency record (metadata only; opaque hashed key).
type residencyEntry struct {
	state      ResidencyState
	tokens     int   // claimed/known prefix length for this key
	warming    int   // count of in-flight requests currently warming this key (conservative reserve)
	lastSeq    int64 // monotone local op sequence (for idempotency / ordering)
	resetEpoch int64 // epoch this entry belongs to; stale if < current epoch
	updatedAt  time.Time
	readyAt    time.Time
}

// Residency is a bounded, thread-safe per-prefix state machine:
//
//	ABSENT -> WARMING (BeginWarm: a cache-eligible request is dispatched to the local runtime)
//	WARMING -> READY  (MarkReady: first upstream token / 2xx completion / verified BlockStored)
//	WARMING -> ABSENT (AbortWarm: pre-first-token failure, cancel-before-ready, non-2xx) — only when
//	                   no OTHER request is still warming the same key
//	READY/WARMING -> ABSENT (Reset: runtime restart / AllBlocksCleared / TTL expiry)
//
// A WARMING prefix is NEVER reported as a reusable hit. Concurrent requests for a warming key reserve
// conservatively (the policy/work-accounting treat WARMING as "no usable match yet").
type Residency struct {
	mu         sync.RWMutex
	now        func() time.Time
	entries    map[string]*residencyEntry
	maxEntries int
	ttl        time.Duration
	resetEpoch int64
	seq        int64
	// counters
	beginWarms  uint64
	readied     uint64
	aborted     uint64
	falseReady  uint64 // MarkReady on a key that was already ABSENT (e.g. aborted then late ready)
	ttlExpired  uint64
	resets      uint64
	lastUpdate  time.Time
}

// NewResidency builds an empty residency tracker.
func NewResidency(maxEntries int, ttl time.Duration) *Residency {
	if maxEntries <= 0 {
		maxEntries = 100_000
	}
	return &Residency{now: time.Now, entries: make(map[string]*residencyEntry), maxEntries: maxEntries, ttl: ttl}
}

// WithClock overrides the clock (tests only).
func (r *Residency) WithClock(f func() time.Time) *Residency {
	r.mu.Lock()
	r.now = f
	r.mu.Unlock()
	return r
}

// BeginWarm records that a cache-eligible request was dispatched to the runtime for `key`. The key
// transitions ABSENT->WARMING (or stays READY if already ready — a re-request of a hot prefix). The
// warming refcount is incremented so concurrent warmers are tracked. Returns the state AFTER begin.
func (r *Residency) BeginWarm(key string, tokens int) ResidencyState {
	if key == "" {
		return StateAbsent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.beginWarms++
	r.lastUpdate = r.now()
	e := r.entries[key]
	if e == nil {
		e = &residencyEntry{state: StateAbsent, resetEpoch: r.resetEpoch}
		r.entries[key] = e
		r.evictIfNeededLocked()
	}
	if tokens > 0 {
		e.tokens = tokens
	}
	e.warming++
	e.lastSeq = r.seq
	e.resetEpoch = r.resetEpoch
	e.updatedAt = r.now()
	if e.state == StateAbsent {
		e.state = StateWarming
	}
	return e.state
}

// MarkReady transitions a key WARMING->READY (first token / 2xx). Idempotent: a second MarkReady for
// an already-READY key is a no-op. A MarkReady for a key that is ABSENT (e.g. it was aborted/reset
// before this readiness signal) is NOT resurrected — counted as a false-ready and ignored, so a
// stale readiness signal cannot revive evicted locality.
func (r *Residency) MarkReady(key string) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.lastUpdate = r.now()
	e := r.entries[key]
	if e == nil || e.state == StateAbsent {
		r.falseReady++
		return
	}
	// drop one warming refcount (this request finished warming)
	if e.warming > 0 {
		e.warming--
	}
	if e.state == StateReady {
		return // idempotent
	}
	e.state = StateReady
	e.lastSeq = r.seq
	e.updatedAt = r.now()
	e.readyAt = r.now()
	r.readied++
}

// AbortWarm handles a warming request that failed before readiness (pre-first-token failure, cancel,
// non-2xx). It decrements the warming refcount; the key returns to ABSENT only when NO other request
// is still warming it AND it is not already READY (a concurrent request may have readied it).
func (r *Residency) AbortWarm(key string) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.lastUpdate = r.now()
	e := r.entries[key]
	if e == nil {
		return
	}
	if e.warming > 0 {
		e.warming--
	}
	if e.state == StateReady {
		return // already readied by a concurrent request; abort of THIS attempt doesn't unready it
	}
	if e.warming == 0 {
		// last warmer failed -> back to absent
		e.state = StateAbsent
		e.lastSeq = r.seq
		e.updatedAt = r.now()
		r.aborted++
	}
}

// Reset wipes all residency (runtime restart / AllBlocksCleared) and bumps the reset epoch.
func (r *Residency) Reset(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = make(map[string]*residencyEntry)
	r.resetEpoch++
	r.resets++
	r.lastUpdate = r.now()
}

// Lookup returns the residency state + READY token count for a key, applying TTL expiry. Only a
// READY (and non-expired) key reports reusable tokens; WARMING/ABSENT report 0.
func (r *Residency) Lookup(key string) (state ResidencyState, readyTokens int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.entries[key]
	if e == nil {
		return StateAbsent, 0
	}
	if r.ttl > 0 && e.state == StateReady && r.now().Sub(e.readyAt) > r.ttl {
		return StateAbsent, 0 // TTL-expired READY -> treat as absent (lazy; SnapshotMeta sweeps counts)
	}
	if e.state == StateReady {
		return StateReady, e.tokens
	}
	return e.state, 0
}

// evictIfNeededLocked enforces maxEntries by dropping the oldest-updated entries. Caller holds wlock.
func (r *Residency) evictIfNeededLocked() {
	over := len(r.entries) - r.maxEntries
	for over > 0 {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range r.entries {
			if first || e.updatedAt.Before(oldest) {
				oldest, oldestKey, first = e.updatedAt, k, false
			}
		}
		if oldestKey == "" {
			return
		}
		delete(r.entries, oldestKey)
		over--
	}
}

// ResidencyStats is a bounded snapshot of residency counters for /v1/cache + tests.
type ResidencyStats struct {
	Ready        int    `json:"ready_entries"`
	Warming      int    `json:"warming_entries"`
	Total        int    `json:"total_entries"`
	ResetEpoch   int64  `json:"reset_epoch"`
	BeginWarms   uint64 `json:"begin_warms_total"`
	Readied      uint64 `json:"readied_total"`
	Aborted      uint64 `json:"aborted_total"`
	FalseReady   uint64 `json:"false_ready_total"`
	Resets       uint64 `json:"resets_total"`
}

// Stats sweeps TTL-expired READY entries (counting them) and returns bounded residency counters.
func (r *Residency) Stats() ResidencyStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	ready, warming := 0, 0
	for k, e := range r.entries {
		if r.ttl > 0 && e.state == StateReady && r.now().Sub(e.readyAt) > r.ttl {
			delete(r.entries, k)
			r.ttlExpired++
			continue
		}
		switch e.state {
		case StateReady:
			ready++
		case StateWarming:
			warming++
		}
	}
	return ResidencyStats{
		Ready: ready, Warming: warming, Total: len(r.entries), ResetEpoch: r.resetEpoch,
		BeginWarms: r.beginWarms, Readied: r.readied, Aborted: r.aborted,
		FalseReady: r.falseReady, Resets: r.resets,
	}
}

// ResetEpoch returns the current epoch.
func (r *Residency) ResetEpoch() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resetEpoch
}

// ReadyDirectory returns opaque key -> ready token count for all READY (non-expired) entries,
// bounded by max (most-recently-updated first). WARMING/ABSENT keys are excluded by construction.
func (r *Residency) ReadyDirectory(max int) map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int)
	if max <= 0 {
		max = r.maxEntries
	}
	type kv struct {
		k   string
		t   time.Time
		tok int
	}
	var cand []kv
	for k, e := range r.entries {
		if e.state != StateReady {
			continue
		}
		if r.ttl > 0 && r.now().Sub(e.readyAt) > r.ttl {
			continue
		}
		cand = append(cand, kv{k, e.updatedAt, e.tokens})
	}
	if len(cand) > max {
		// keep most-recent
		for i := 1; i < len(cand); i++ {
			for j := i; j > 0 && cand[j].t.After(cand[j-1].t); j-- {
				cand[j], cand[j-1] = cand[j-1], cand[j]
			}
		}
		cand = cand[:max]
	}
	for _, c := range cand {
		out[c.k] = c.tok
	}
	return out
}
