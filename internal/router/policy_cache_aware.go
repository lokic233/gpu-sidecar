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
	// Decode: per-output-token cost (ms/token). Uses measured service rate when available.
	DecodeMsPerToken float64
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
	// DefaultServiceRateTokPerSec: fallback decode rate when no measured rate exists (=> DecodeMsPerToken).
	DefaultServiceRateTokPerSec float64
}

// DefaultCacheAwareConfig returns documented, conservative defaults. These are first-order estimates
// for a small model (Qwen2.5-0.5B class) on the test GPUs; they are coefficients of a transparent
// cost model, NOT tuned reward weights. They are meant to be a strong analytical BASELINE over which
// PPO learns a residual — not a final calibrated latency predictor.
func DefaultCacheAwareConfig() CacheAwareConfig {
	return CacheAwareConfig{
		PrefillMsPerToken:           0.05, // ~20k uncached prompt tok/s prefill (order-of-magnitude)
		DecodeMsPerToken:            0.30, // ~3.3k tok/s decode fallback when no measured rate
		QueueMsPerQueued:            8.0,  // each already-queued request adds ~8ms head-of-line
		RuntimeWaitingMsPerReq:      6.0,  // each vLLM-waiting request adds ~6ms
		RuntimeRunningMsPerReq:      1.5,  // each running seq adds minor batched contention
		CacheStalenessPenaltyMs:     25.0, // full penalty when confidence=0 but plane is "supported"
		KVPressurePenaltyMs:         40.0, // full penalty when KV headroom=0
		ConfidenceFloor:             0.30, // ignore locality below 30% confidence
		DefaultDecodeTokens:         128,
		DefaultServiceRateTokPerSec: 0,    // 0 => use DecodeMsPerToken
	}
}

// CacheLocator resolves the matched prefix-token count for a (backend, request) pair from the
// router's materialized cache directory (O(1) local lookup; NO per-request network query). The
// Registry implements this.
type CacheLocator interface {
	LookupPrefixTokens(backendID, prefixKeyHash string) int
}

// CacheAwarePolicy implements cache_aware_estimated_finish. It estimates each eligible backend's
// finish time as:
//
//	finish = queue_delay + prefill(uncached_prompt_tokens) + decode(expected_output)
//	       + cache_staleness_penalty + kv_pressure_penalty
//
// and selects the minimum. A cache-hot but overloaded backend can still lose (its queue/KV terms
// dominate); a lightly loaded cache-hot backend normally wins (its prefill term shrinks). When cache
// observation is unsupported or stale, the locality term is dropped and the policy reduces to the
// existing load-aware estimate. It NEVER optimizes raw cache-hit rate and adds NO equal-utilization
// reward.
type CacheAwarePolicy struct {
	cfg     CacheAwareConfig
	locator CacheLocator
}

// NewCacheAwarePolicy builds the policy. locator may be nil (then prefix matching falls back to the
// per-request PrefixMatchedTokens already set on BackendState, or 0).
func NewCacheAwarePolicy(cfg CacheAwareConfig, locator CacheLocator) *CacheAwarePolicy {
	return &CacheAwarePolicy{cfg: cfg, locator: locator}
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
}

// matchedTokensFor resolves matched prefix tokens for a backend honoring confidence/support gating.
func (p *CacheAwarePolicy) matchedTokensFor(req RequestFeatures, b BackendState) (int, bool, float64) {
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
	matched := b.PrefixMatchedTokens
	if p.locator != nil {
		if m := p.locator.LookupPrefixTokens(b.Backend.ID, req.PrefixKeyHash); m > 0 {
			matched = m
		}
	}
	// matched cannot exceed the request's claimed prefix length or its input length.
	if req.PrefixTokens > 0 && matched > req.PrefixTokens {
		matched = req.PrefixTokens
	}
	if req.InputLenEst > 0 && matched > req.InputLenEst {
		matched = req.InputLenEst
	}
	return matched, true, b.CacheConfidence
}

func (p *CacheAwarePolicy) decodeMsPerToken(b BackendState) float64 {
	// Prefer measured service rate (tokens/s) when reliable; else configured fallback.
	if b.ServiceRateSupported && b.GenTokensPerSec > 1 {
		return 1000.0 / b.GenTokensPerSec
	}
	if p.cfg.DefaultServiceRateTokPerSec > 1 {
		return 1000.0 / p.cfg.DefaultServiceRateTokPerSec
	}
	return p.cfg.DecodeMsPerToken
}

func (p *CacheAwarePolicy) cost(req RequestFeatures, b BackendState) candidateCost {
	c := candidateCost{backendID: b.Backend.ID}
	inputLen := req.InputLenEst
	if inputLen < 0 {
		inputLen = 0
	}
	matched, used, conf := p.matchedTokensFor(req, b)
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

	c.estPrefillMs = float64(uncached) * p.cfg.PrefillMsPerToken
	c.estDecodeMs = float64(out) * p.decodeMsPerToken(b)
	c.estQueueMs = float64(b.QueueDepth+b.QueueInflight)*p.cfg.QueueMsPerQueued +
		b.RuntimeWaiting*p.cfg.RuntimeWaitingMsPerReq +
		b.RuntimeRunning*p.cfg.RuntimeRunningMsPerReq

	// Cache staleness penalty: only when the plane is supported but we could not trust locality.
	// (Discourages a backend whose cache we cannot verify, scaled by how unconfident we are.)
	if b.CacheObservationSupported {
		c.stalenessPenaltyMs = p.cfg.CacheStalenessPenaltyMs * (1 - clamp01(b.CacheConfidence))
	}
	// KV / eviction pressure penalty: only when KV headroom is measurable.
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
		costs = append(costs, p.cost(req, b))
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
	reason := fmt.Sprintf("min_finish=%.1fms(q=%.1f+prefill=%.1f[uncached=%d/match=%d]+decode=%.1f+stale=%.1f+kv=%.1f)",
		best.finishMs, best.estQueueMs, best.estPrefillMs, best.uncachedPromptTokens,
		best.matchedPrefixTokens, best.estDecodeMs, best.stalenessPenaltyMs, best.kvPenaltyMs)
	return RouteDecision{
		BackendID: best.backendID, PolicyName: "cache_aware_estimated_finish", PolicyVersion: "1",
		Reason: reason,
	}, nil
}

// ScoreBreakdown returns the full per-backend cost breakdown for the request (for trajectory/RL
// state emission). The slice is sorted by ascending finish time (selected backend first).
func (p *CacheAwarePolicy) ScoreBreakdown(req RequestFeatures, snap *BackendSnapshot) []CandidateScore {
	e := eligible(snap)
	out := make([]CandidateScore, 0, len(e))
	for _, b := range e {
		c := p.cost(req, b)
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
			EstPrefillSavedMs:    float64(c.matchedPrefixTokens) * p.cfg.PrefillMsPerToken,
			FinalScoreMs:         c.finishMs,
			CacheUsed:            c.cacheUsed,
			CacheConfidence:      c.cacheConfidence,
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
}

// CacheAffinityOnlyPolicy is a deliberately naive baseline for the experimental comparison: it
// maximizes matched prefix tokens (herds onto cache-hot backends), ignoring congestion. It exists to
// demonstrate why affinity-only routing is bad under load. NOT for production use.
type CacheAffinityOnlyPolicy struct {
	locator CacheLocator
}

func NewCacheAffinityOnlyPolicy(locator CacheLocator) *CacheAffinityOnlyPolicy {
	return &CacheAffinityOnlyPolicy{locator: locator}
}

func (p *CacheAffinityOnlyPolicy) SelectBackend(req RequestFeatures, snap *BackendSnapshot) (RouteDecision, error) {
	e := eligible(snap)
	if len(e) == 0 {
		return RouteDecision{}, ErrNoEligibleBackend
	}
	bestIdx := 0
	bestMatch := -1
	for i, b := range e {
		m := b.PrefixMatchedTokens
		if p.locator != nil && req.PrefixKeyHash != "" && b.CacheObservationSupported {
			if v := p.locator.LookupPrefixTokens(b.Backend.ID, req.PrefixKeyHash); v > m {
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
		PolicyVersion: "1", Reason: fmt.Sprintf("max_matched_prefix=%d", bestMatch)}, nil
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
