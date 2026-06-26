package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// ExplicitProvider implements deterministic, runtime-independent cache-locality observation driven
// by opaque experiment prefix keys (carried on requests as X-Cache-Prefix-Key, already HASHED by
// the proxy before they ever reach this provider).
//
// Semantics modeled to mirror real prefix caching WITHOUT touching the runtime:
//   - The FIRST time a backend serves a given prefix key, there is no local cache yet -> no match
//     (a cold prefix must be prefilled once). The provider then records the key as "stored".
//   - SUBSEQUENT requests for the same key on the same backend match (warm) -> matched_prefix_tokens
//     = min(claimed prefix tokens, recorded tokens), with confidence from index freshness.
//   - A cache reset / runtime restart / TTL expiry invalidates the recorded key (back to cold).
//
// This is explicitly NON-PRODUCTION / experiment-only. It lets us generate fixed prefix groups
// (hot/warm/unique) and verify cache-aware routing deterministically on BOTH H100 and MI350X,
// before (and independently of) any native vLLM block matching.
type ExplicitProvider struct {
	cfg   ProviderConfig
	index *Index

	// monotone synthetic sequence for index bookkeeping (explicit mode has no native seq).
	seq atomic.Int64

	mu          sync.Mutex
	kvHeadroom  float64
	kvSupported bool
	started     bool
	resetEpoch  int64
}

// NewExplicitProvider builds an explicit-mode provider.
func NewExplicitProvider(cfg ProviderConfig) *ExplicitProvider {
	if cfg.Index.MaxEntries == 0 {
		cfg.Index = DefaultIndexConfig()
	}
	return &ExplicitProvider{cfg: cfg, index: NewIndex(cfg.Index)}
}

// IndexForTest exposes the underlying index (tests only).
func (p *ExplicitProvider) IndexForTest() *Index { return p.index }

func (p *ExplicitProvider) Mode() Mode { return ModeExplicit }

func (p *ExplicitProvider) Start(context.Context) error {
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	return nil
}

func (p *ExplicitProvider) Stop() error { return nil }

// OnRuntimeRestart invalidates all recorded locality: a restarted runtime lost its KV cache.
func (p *ExplicitProvider) OnRuntimeRestart() {
	p.index.Reset("runtime_restart")
	p.mu.Lock()
	p.resetEpoch++
	p.mu.Unlock()
}

func (p *ExplicitProvider) SetKVHeadroom(h float64, supported bool) {
	p.mu.Lock()
	p.kvHeadroom = h
	p.kvSupported = supported
	p.mu.Unlock()
}

// Observe records that this backend has now served (and therefore cached) the given prefix key for
// the given token count. Called by the proxy AFTER a request for a cache-eligible prefix is admitted
// to the runtime, so the NEXT request for the same key sees a warm cache. Idempotent per key.
//
// keyHash MUST already be hashed (never the raw key). tokens is the claimed prefix length (bounded).
func (p *ExplicitProvider) Observe(keyHash string, tokens int) {
	if keyHash == "" || tokens <= 0 {
		return
	}
	s := p.seq.Add(1)
	// matchedTokens recorded = claimed prefix tokens (the explicit upper bound for this key).
	p.index.ApplyStore(s, keyHash, "", tokens, 0)
}

// Lookup answers whether this backend has a warm cache for the request's prefix key.
func (p *ExplicitProvider) Lookup(q PrefixQuery) MatchResult {
	if !q.Eligible() {
		// not a cache-eligible request: supported plane, but no match. confidence 1 (we are certain
		// there is no reusable prefix), 0 matched tokens.
		return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: 0,
			Confidence: 1, Reason: "no_prefix_key"}
	}
	recorded, conf, ageMs := p.index.LookupKey(q.PrefixKeyHash)
	if recorded == 0 {
		// cold prefix on this backend (never served here, or invalidated/stale).
		return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: 0,
			Confidence: conf, SnapshotAgeMs: ageMs, Reason: "cold_prefix"}
	}
	matched := q.PrefixTokens
	if recorded < matched {
		matched = recorded
	}
	if conf <= 0 {
		// stale: do not claim a match even though we have a record.
		return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: 0,
			Confidence: 0, SnapshotAgeMs: ageMs, Reason: "stale_record"}
	}
	return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: matched,
		Confidence: conf, SnapshotAgeMs: ageMs, Reason: "warm_prefix"}
}

func (p *ExplicitProvider) Snapshot() Snapshot {
	s := p.index.SnapshotMeta()
	s.Enabled = true
	s.Provider = string(ModeExplicit)
	s.Supported = true
	s.MatchSupported = true
	p.mu.Lock()
	s.KVHeadroom = p.kvHeadroom
	s.KVHeadroomSupported = p.kvSupported
	p.mu.Unlock()
	// explicit mode is "ready" once started, even before the first key is observed (it can always
	// answer "cold" honestly). But Index.Ready stays false until an event lands; OR-in started.
	if !s.Ready {
		p.mu.Lock()
		s.Ready = p.started
		p.mu.Unlock()
	}
	if s.SnapshotAgeMs < 0 {
		s.SnapshotAgeMs = 0
	}
	s.UpdatedAt = nz(s.UpdatedAt)
	return s
}

// Directory exposes the bounded prefix directory (match-capable in explicit mode).
func (p *ExplicitProvider) Directory(max int) map[string]int { return p.index.Directory(max) }

func nz(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
