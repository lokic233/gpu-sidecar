package cache

import (
	"context"
	"sync"
	"time"
)

// ExplicitProvider implements deterministic, runtime-independent cache-locality observation driven
// by opaque experiment prefix keys (carried on requests as X-Cache-Prefix-Key, already HASHED by
// the proxy before they ever reach this provider).
//
// Round-5 hardening: locality now follows a conservative per-prefix residency STATE MACHINE
// (ABSENT -> WARMING -> READY) instead of a single binary Observe(). A prefix is only reported as a
// reusable hit once it is READY (first token / successful completion). A request that fails before
// readiness, is cancelled, or hits a runtime restart returns the prefix toward ABSENT and is NEVER
// counted as a warm hit. See cache_residency_contract.md.
//
// This is explicitly NON-PRODUCTION / experiment-only. The experiment harness MUST map each explicit
// prefix key to a genuinely identical real prompt-token prefix (see experiment_protocol.md), so the
// synthetic key grouping corresponds to real shared content.
type ExplicitProvider struct {
	cfg  ProviderConfig
	res  *Residency

	mu          sync.Mutex
	kvHeadroom  float64
	kvSupported bool
	started     bool
}

// NewExplicitProvider builds an explicit-mode provider backed by a residency state machine.
func NewExplicitProvider(cfg ProviderConfig) *ExplicitProvider {
	if cfg.Index.MaxEntries == 0 {
		cfg.Index = DefaultIndexConfig()
	}
	return &ExplicitProvider{cfg: cfg, res: NewResidency(cfg.Index.MaxEntries, cfg.Index.EntryTTL)}
}

// ResidencyForTest exposes the residency tracker (tests only).
func (p *ExplicitProvider) ResidencyForTest() *Residency { return p.res }

func (p *ExplicitProvider) Mode() Mode { return ModeExplicit }

func (p *ExplicitProvider) Start(context.Context) error {
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	return nil
}

func (p *ExplicitProvider) Stop() error { return nil }

// OnRuntimeRestart invalidates all recorded locality: a restarted runtime lost its KV cache.
func (p *ExplicitProvider) OnRuntimeRestart() { p.res.Reset("runtime_restart") }

func (p *ExplicitProvider) SetKVHeadroom(h float64, supported bool) {
	p.mu.Lock()
	p.kvHeadroom = h
	p.kvSupported = supported
	p.mu.Unlock()
}

// BeginWarm marks a cache-eligible prefix as WARMING (request dispatched to the local runtime).
func (p *ExplicitProvider) BeginWarm(keyHash string, tokens int) {
	if keyHash == "" || tokens <= 0 {
		return
	}
	p.res.BeginWarm(keyHash, tokens)
}

// MarkReady promotes a WARMING prefix to READY (first token / successful completion).
func (p *ExplicitProvider) MarkReady(keyHash string) {
	if keyHash == "" {
		return
	}
	p.res.MarkReady(keyHash)
}

// AbortWarm returns a WARMING prefix toward ABSENT (pre-first-token failure / cancel / non-2xx).
func (p *ExplicitProvider) AbortWarm(keyHash string) {
	if keyHash == "" {
		return
	}
	p.res.AbortWarm(keyHash)
}

// Reset wipes locality (runtime restart / cache clear).
func (p *ExplicitProvider) Reset(reason string) { p.res.Reset(reason) }

// Lookup answers whether this backend has a READY (reusable) cache for the request's prefix key.
// WARMING and ABSENT both report 0 reusable tokens — a warming prefix is NOT a hit.
func (p *ExplicitProvider) Lookup(q PrefixQuery) MatchResult {
	if !q.Eligible() {
		// not a cache-eligible request: supported plane, certain there is no reusable prefix.
		return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: 0,
			Confidence: 1, Reason: "no_prefix_key"}
	}
	state, readyTokens := p.res.Lookup(q.PrefixKeyHash)
	switch state {
	case StateReady:
		matched := q.PrefixTokens
		if readyTokens > 0 && readyTokens < matched {
			matched = readyTokens
		}
		return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: matched,
			Confidence: 1, Reason: "ready_prefix"}
	case StateWarming:
		// warming is NOT a reusable hit; report 0 matched but state is observable for RL/debug.
		return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: 0,
			Confidence: 1, Reason: "warming_prefix"}
	default:
		return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: 0,
			Confidence: 1, Reason: "absent_prefix"}
	}
}

// LookupState exposes the raw residency state for a key (for RL state emission / debug).
func (p *ExplicitProvider) LookupState(keyHash string) (ready bool, readyTokens int) {
	state, tokens := p.res.Lookup(keyHash)
	return state == StateReady, tokens
}

// ResidencyStateOf returns the full residency state for a key (tests / trajectory detail).
func (p *ExplicitProvider) ResidencyStateOf(keyHash string) (ResidencyState, int) {
	return p.res.Lookup(keyHash)
}

func (p *ExplicitProvider) Snapshot() Snapshot {
	rs := p.res.Stats()
	p.mu.Lock()
	started := p.started
	kvHr, kvSup := p.kvHeadroom, p.kvSupported
	p.mu.Unlock()
	return Snapshot{
		Enabled: true, Provider: string(ModeExplicit), Supported: true, MatchSupported: true,
		Ready: started, Confidence: 1, SnapshotAgeMs: 0,
		LastEventSequence: int64(rs.BeginWarms + rs.Readied + rs.Aborted),
		CacheResetEpoch:   rs.ResetEpoch,
		IndexEntries:      rs.Ready, // only READY entries are reusable directory members
		IndexMaxEntries:   p.cfg.Index.MaxEntries,
		KVHeadroom:        kvHr, KVHeadroomSupported: kvSup,
		ResidencyReady: rs.Ready, ResidencyWarming: rs.Warming, ResidencyTotal: rs.Total,
		FalseReadyTotal: rs.FalseReady, ResetsTotal: rs.Resets,
		UpdatedAt: time.Now(),
	}
}

// Directory exposes ONLY READY prefixes (WARMING/ABSENT excluded) — a warming prefix must never
// appear as a routable cache hit.
func (p *ExplicitProvider) Directory(max int) map[string]int { return p.res.ReadyDirectory(max) }

