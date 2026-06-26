package router

import (
	"fmt"
	"sort"
)

// CacheAwareConfig holds the EXPLICIT, documented coefficients of the cache-aware analytical policy.
// No magic constants are buried in the scoring function; every term is named and defaulted here and
// can be overridden per deployment. Times are in milliseconds. The policy estimates a finish time
// per backend and picks the minimum; ties broken by stable logical backend id (order-independent).
type CacheAwareConfig struct {
	// Prefill: per-uncached-prompt-token cost (ms/token). Dominant term for long uncached inputs.
	PrefillMsPerToken float64
	// FallbackDecodeMsPerToken: GLOBAL conservative decode cost (ms/token) used when a backend has NO
	// configured service profile. Deliberately NOT derived from live aggregate throughput (see §P1).
	FallbackDecodeMsPerToken float64
	// QueueDelay: per-queued-or-inflight unit cost (ms). Models head-of-line delay from the host
	// admission queue AND the vLLM runtime waiting count.
	QueueMsPerQueued        float64
	RuntimeWaitingMsPerReq  float64
	RuntimeRunningMsPerReq  float64
	// CacheStalenessPenaltyMs: added when cache observation is supported but low-confidence/stale,
	// scaled by (1-confidence). Discourages trusting a backend whose locality we cannot verify.
	CacheStalenessPenaltyMs float64
	// KVPressurePenaltyMs: added proportionally to (1 - KVHeadroom). A backend near KV exhaustion is
	// penalized because it will preempt/evict (hurting everyone). Eviction pressure proxy.
	KVPressurePenaltyMs float64
	// ConfidenceFloor: below this cache confidence, matched prefix tokens are IGNORED (treated as 0)
	// — we fall back to the load-only estimate for that backend. Safety against stale locality.
	ConfidenceFloor float64
	// DefaultDecodeTokens: assumed output length when the request does not specify max_tokens.
	DefaultDecodeTokens int
}

// DefaultCacheAwareConfig returns documented, conservative defaults. These are first-order estimates
// for a small model (Qwen2.5-0.5B class) on the test GPUs; they are coefficients of a transparent
// cost model, NOT tuned reward weights. They are meant to be a strong analytical BASELINE over which
// PPO learns a residual — not a final calibrated latency predictor.
func DefaultCacheAwareConfig() CacheAwareConfig {
	return CacheAwareConfig{
		PrefillMsPerToken:        0.05, // ~20k uncached prompt tok/s prefill (order-of-magnitude)
		FallbackDecodeMsPerToken: 0.30, // ~3.3k tok/s GLOBAL fallback when no per-backend profile
		QueueMsPerQueued:         8.0,  // each already-queued request adds ~8ms head-of-line
		RuntimeWaitingMsPerReq:   6.0,  // each vLLM-waiting request adds ~6ms
		RuntimeRunningMsPerReq:   1.5,  // each running seq adds minor batched contention
		CacheStalenessPenaltyMs:  25.0, // full penalty when confidence=0 but plane is "supported"
		KVPressurePenaltyMs:      40.0, // full penalty when KV headroom=0
		ConfidenceFloor:          0.30, // ignore locality below 30% confidence
		DefaultDecodeTokens:      128,
	}
}

// BackendProfile is a backend's SLOW/STATIC service capability — calibrated or configured offline,
// NOT derived from live aggregate throughput (see §P1: using live aggregate Δtokens/Δt as
// per-request decode speed creates a busy→"faster"→busier feedback loop under continuous batching).
// A minimal profile is just decode/prefill ms-per-token; richer curves can be added later.
type BackendProfile struct {
	DecodeMsPerToken  float64 `json:"decode_ms_per_token"`  // per-request decode cost
	PrefillMsPerToken float64 `json:"prefill_ms_per_token"` // per-uncached-token prefill cost (0 => use config)
	Version           string  `json:"version,omitempty"`
	// Confidence in [0,1]; informational. 0 / absent => the global fallback is used (recorded).
	Confidence float64 `json:"confidence,omitempty"`
}

// CacheAwarePolicy implements cache_aware_estimated_finish. It estimates each eligible backend's
// finish time as:
//
//	finish = queue_delay + prefill(uncached_prompt_tokens) + decode(expected_output)
//	       + cache_staleness_penalty + kv_pressure_penalty
//
// Locality is resolved from the SAME immutable snapshot passed to SelectBackend (no mutable global /
// no cross-generation mix). Decode cost comes from a per-backend service PROFILE (or a global
// fallback) — never from live aggregate throughput. A cache-hot but overloaded backend can still
// lose; an unsupported/stale cache observation drops the locality term. No raw-hit-rate or
// utilization-fairness reward.
type CacheAwarePolicy struct {
	cfg      CacheAwareConfig
	profiles map[string]BackendProfile // backendID -> static service profile (may be nil/empty)
}

// NewCacheAwarePolicy builds the policy. profiles may be nil/empty (then the global fallback decode
// cost is used and recorded as a fallback per candidate).
func NewCacheAwarePolicy(cfg CacheAwareConfig, profiles map[string]BackendProfile) *CacheAwarePolicy {
	return &CacheAwarePolicy{cfg: cfg, profiles: profiles}
}

// candidateCost is the breakdown for one backend (also surfaced into trajectory state for RL).
type candidateCost struct {
	backendID            string
	uncachedPromptTokens int
	matchedPrefixTokens  int
	matchRatio           float64
	estPrefillMs         float64
	estDecodeMs          float64
	estQueueMs           float64
	stalenessPenaltyMs   float64
	kvPenaltyMs          float64
	finishMs             float64
	cacheUsed            bool
	cacheConfidence      float64
	profileFallback      bool // true when no per-backend profile existed (global fallback used)
}

// matchedTokensFor resolves matched prefix tokens for a backend FROM THE SNAPSHOT, honoring
// support/confidence gating. Only READY locality in the snapshot directory counts (a WARMING prefix
// never appears there). Returns (matched, localityUsable, confidence).
func (p *CacheAwarePolicy) matchedTokensFor(req RequestFeatures, b BackendState, snap *BackendSnapshot) (int, bool, float64) {
	// Unsupported cache observation => never a real zero-match; just no locality (fall back).
	if !b.CacheObservationSupported {
		return 0, false, 0
	}
	// Low/stale confidence => ignore locality (safety).
	if b.CacheConfidence < p.cfg.ConfidenceFloor {
		return 0, false, b.CacheConfidence
	}
	if !req.CacheEligible || req.PrefixKeyHash == "" {
		return 0, true, b.CacheConfidence
	}
	// Resolve from the SAME snapshot (atomic generation) — not a mutable global directory.
	matched := snap.LookupPrefixTokens(b.Backend.ID, req.PrefixKeyHash)
	if matched == 0 && b.PrefixMatchedTokens > 0 {
		matched = b.PrefixMatchedTokens // test/explicit pre-set fallback
	}
	if req.PrefixTokens > 0 && matched > req.PrefixTokens {
		matched = req.PrefixTokens
	}
	if req.InputLenEst > 0 && matched > req.InputLenEst {
		matched = req.InputLenEst
	}
	return matched, true, b.CacheConfidence
}

// decodeMsPerToken returns the per-request decode cost from the backend's static PROFILE, or the
// global fallback. It NEVER uses live aggregate throughput (which would create a busy→busier loop).
func (p *CacheAwarePolicy) decodeMsPerToken(b BackendState) (float64, bool) {
	if p.profiles != nil {
		if prof, ok := p.profiles[b.Backend.ID]; ok && prof.DecodeMsPerToken > 0 {
			return prof.DecodeMsPerToken, false
		}
	}
	return p.cfg.FallbackDecodeMsPerToken, true // fallback used (recorded)
}

func (p *CacheAwarePolicy) prefillMsPerToken(b BackendState) float64 {
	if p.profiles != nil {
		if prof, ok := p.profiles[b.Backend.ID]; ok && prof.PrefillMsPerToken > 0 {
			return prof.PrefillMsPerToken
		}
	}
	return p.cfg.PrefillMsPerToken
}

func (p *CacheAwarePolicy) cost(req RequestFeatures, b BackendState, snap *BackendSnapshot) candidateCost {
	c := candidateCost{backendID: b.Backend.ID}
	inputLen := req.InputLenEst
	if inputLen < 0 {
		inputLen = 0
	}
	matched, used, conf := p.matchedTokensFor(req, b, snap)
	c.matchedPrefixTokens = matched
	c.cacheUsed = used && matched > 0
	c.cacheConfidence = conf

	uncached := inputLen - matched
	if uncached < 0 {
		uncached = 0
	}
	c.uncachedPromptTokens = uncached
	if inputLen > 0 {
		c.matchRatio = float64(matched) / float64(inputLen)
	}

	out := req.RequestedOutput
	if out <= 0 {
		out = p.cfg.DefaultDecodeTokens
	}

	decodeMs, fallback := p.decodeMsPerToken(b)
	c.profileFallback = fallback
	c.estPrefillMs = float64(uncached) * p.prefillMsPerToken(b)
	c.estDecodeMs = float64(out) * decodeMs
	// Live CONGESTION (not capability) drives the queue term: queued + inflight + runtime waiting/
	// running. A growing backlog raises this term, so a busy backend becomes LESS attractive — the
	// opposite of the aggregate-throughput feedback loop we removed.
	c.estQueueMs = float64(b.QueueDepth+b.QueueInflight)*p.cfg.QueueMsPerQueued +
		b.RuntimeWaiting*p.cfg.RuntimeWaitingMsPerReq +
		b.RuntimeRunning*p.cfg.RuntimeRunningMsPerReq

	if b.CacheObservationSupported {
		c.stalenessPenaltyMs = p.cfg.CacheStalenessPenaltyMs * (1 - clamp01(b.CacheConfidence))
	}
	if b.KVHeadroomSupported {
		c.kvPenaltyMs = p.cfg.KVPressurePenaltyMs * (1 - clamp01(b.KVHeadroom))
	}

	c.finishMs = c.estQueueMs + c.estPrefillMs + c.estDecodeMs + c.stalenessPenaltyMs + c.kvPenaltyMs
	return c
}

// SelectBackend implements RoutingPolicy.
func (p *CacheAwarePolicy) SelectBackend(req RequestFeatures, snap *BackendSnapshot) (RouteDecision, error) {
	e := eligible(snap)
	if len(e) == 0 {
		return RouteDecision{}, ErrNoEligibleBackend
	}
	costs := make([]candidateCost, 0, len(e))
	for _, b := range e {
		costs = append(costs, p.cost(req, b, snap))
	}
	// Order-independence: pick min finishMs; break ties by logical backend id (lexicographic) so the
	// chosen LOGICAL backend never depends on snapshot iteration order.
	sort.Slice(costs, func(i, j int) bool {
		if costs[i].finishMs != costs[j].finishMs {
			return costs[i].finishMs < costs[j].finishMs
		}
		return costs[i].backendID < costs[j].backendID
	})
	best := costs[0]
	reason := fmt.Sprintf("min_finish=%.1fms(q=%.1f+prefill=%.1f[uncached=%d/match=%d]+decode=%.1f+stale=%.1f+kv=%.1f%s)",
		best.finishMs, best.estQueueMs, best.estPrefillMs, best.uncachedPromptTokens,
		best.matchedPrefixTokens, best.estDecodeMs, best.stalenessPenaltyMs, best.kvPenaltyMs,
		fallbackTag(best.profileFallback))
	return RouteDecision{
		BackendID: best.backendID, PolicyName: "cache_aware_estimated_finish", PolicyVersion: "2",
		Reason: reason,
	}, nil
}

func fallbackTag(fb bool) string {
	if fb {
		return "+profile_fallback"
	}
	return ""
}

// ScoreBreakdown returns the full per-backend cost breakdown for the request (for trajectory/RL
// state emission). The slice is sorted by ascending finish time (selected backend first).
func (p *CacheAwarePolicy) ScoreBreakdown(req RequestFeatures, snap *BackendSnapshot) []CandidateScore {
	e := eligible(snap)
	out := make([]CandidateScore, 0, len(e))
	for _, b := range e {
		c := p.cost(req, b, snap)
		out = append(out, CandidateScore{
			BackendID:            c.backendID,
			UncachedPromptTokens: c.uncachedPromptTokens,
			MatchedPrefixTokens:  c.matchedPrefixTokens,
			MatchRatio:           c.matchRatio,
			EstQueueMs:           c.estQueueMs,
			EstPrefillMs:         c.estPrefillMs,
			EstDecodeMs:          c.estDecodeMs,
			StalenessPenaltyMs:   c.stalenessPenaltyMs,
			KVPenaltyMs:          c.kvPenaltyMs,
			EstPrefillSavedMs:    float64(c.matchedPrefixTokens) * p.prefillMsPerToken(b),
			FinalScoreMs:         c.finishMs,
			CacheUsed:            c.cacheUsed,
			CacheConfidence:      c.cacheConfidence,
			ProfileFallback:      c.profileFallback,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FinalScoreMs != out[j].FinalScoreMs {
			return out[i].FinalScoreMs < out[j].FinalScoreMs
		}
		return out[i].BackendID < out[j].BackendID
	})
	return out
}

// CandidateScore is the per-backend analytical breakdown surfaced to the RL trajectory state. It is
// the exact "base score" Liangqi's PPO learns a residual over (final_score = FinalScoreMs + residual).
type CandidateScore struct {
	BackendID            string  `json:"backend_id"`
	UncachedPromptTokens int     `json:"uncached_prompt_tokens"`
	MatchedPrefixTokens  int     `json:"matched_prefix_tokens"`
	MatchRatio           float64 `json:"match_ratio"`
	EstQueueMs           float64 `json:"est_queue_ms"`
	EstPrefillMs         float64 `json:"est_prefill_ms"`
	EstDecodeMs          float64 `json:"est_decode_ms"`
	StalenessPenaltyMs   float64 `json:"cache_staleness_penalty_ms"`
	KVPenaltyMs          float64 `json:"kv_pressure_penalty_ms"`
	EstPrefillSavedMs    float64 `json:"estimated_prefill_saved_ms"`
	FinalScoreMs         float64 `json:"final_analytical_score_ms"`
	CacheUsed            bool    `json:"cache_used"`
	CacheConfidence      float64 `json:"cache_confidence"`
	ProfileFallback      bool    `json:"profile_fallback"`
}

// CacheAffinityOnlyPolicy is a deliberately naive baseline for the experimental comparison: it
// maximizes matched prefix tokens (herds onto cache-hot backends), ignoring congestion. It exists to
// demonstrate why affinity-only routing is bad under load. NOT for production use.
type CacheAffinityOnlyPolicy struct{}

func NewCacheAffinityOnlyPolicy() *CacheAffinityOnlyPolicy { return &CacheAffinityOnlyPolicy{} }

func (p *CacheAffinityOnlyPolicy) SelectBackend(req RequestFeatures, snap *BackendSnapshot) (RouteDecision, error) {
	e := eligible(snap)
	if len(e) == 0 {
		return RouteDecision{}, ErrNoEligibleBackend
	}
	bestIdx := 0
	bestMatch := -1
	for i, b := range e {
		m := b.PrefixMatchedTokens
		if req.PrefixKeyHash != "" && b.CacheObservationSupported {
			if v := snap.LookupPrefixTokens(b.Backend.ID, req.PrefixKeyHash); v > m {
				m = v
			}
		}
		// order-independent tie-break by backend id
		if m > bestMatch || (m == bestMatch && b.Backend.ID < e[bestIdx].Backend.ID) {
			bestMatch = m
			bestIdx = i
		}
	}
	return RouteDecision{BackendID: e[bestIdx].Backend.ID, PolicyName: "cache_affinity_only",
		PolicyVersion: "2", Reason: fmt.Sprintf("max_matched_prefix=%d", bestMatch)}, nil
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
