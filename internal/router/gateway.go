package router

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/trajectory"
)

// Gateway is the experimental Global Router Gateway.
type Gateway struct {
	reg     *Registry
	policy  RoutingPolicy
	emitter *trajectory.Emitter
	client  *http.Client // long-timeout streaming client to sidecars

	maxRetries int // cross-backend retries BEFORE first byte (default 1)
	reqSeq     atomic.Uint64
}

func NewGateway(reg *Registry, policy RoutingPolicy, emitter *trajectory.Emitter, maxRetries int) *Gateway {
	return &Gateway{
		reg: reg, policy: policy, emitter: emitter, maxRetries: maxRetries,
		client: &http.Client{
			Timeout: 0, // streaming: no global deadline; rely on ctx + sidecar
			Transport: &http.Transport{
				MaxIdleConns: 128, MaxIdleConnsPerHost: 128, IdleConnTimeout: 90 * time.Second,
				DisableCompression: true,
			},
		},
	}
}

func (g *Gateway) emit(kind, reqID, routeID, backendID string, fields map[string]any) {
	if g.emitter == nil {
		return
	}
	g.emitter.Emit(trajectory.Event{
		Kind: kind, Source: "router", RequestID: reqID, RouteID: routeID, BackendID: backendID,
		Wall: time.Now(), Fields: fields,
	})
}

func (g *Gateway) backendByID(snap *BackendSnapshot, id string) *BackendState {
	for i := range snap.Backends {
		if snap.Backends[i].Backend.ID == id {
			return &snap.Backends[i]
		}
	}
	return nil
}

// ChatCompletions is the client-facing OpenAI-compatible endpoint.
func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	clientStart := time.Now()
	reqID := orGen(r.Header.Get("X-Request-ID"), "creq")
	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "READ_BODY", reqID, err.Error())
		return
	}
	stream := detectStream(body)
	prefixKeyHash, prefixTokens, sessionKeyHash := extractPrefixFeatures(r)
	feat := RequestFeatures{
		RequestID: reqID, Model: extractModel(body), Stream: stream,
		InputLenEst: estimateTokens(body), RequestedOutput: extractMaxTokens(body),
		SLOClass:       r.Header.Get("X-SLO-Class"),
		PrefixKeyHash:  prefixKeyHash,
		PrefixTokens:   prefixTokens,
		CacheEligible:  prefixKeyHash != "",
		SessionKeyHash: sessionKeyHash,
	}
	recvFields := map[string]any{"stream": stream, "model": feat.Model, "input_len_est": feat.InputLenEst}
	if feat.CacheEligible {
		recvFields["cache_eligible"] = true
		recvFields["prefix_key_hash"] = prefixKeyHash // hashed; never the raw key
		recvFields["prefix_tokens"] = prefixTokens
	}
	g.emit("REQUEST_RECEIVED", reqID, "", "", recvFields)

	snap := g.reg.Snapshot()
	g.emit("BACKEND_SNAPSHOT_READ", reqID, "", "", map[string]any{
		"snapshot_age_ms": ms(time.Since(snap.Timestamp)), "n_backends": len(snap.Backends)})
	// Emit the full per-candidate analytical RL state (every routing decision can be reconstructed).
	g.emitCandidateState(reqID, feat, snap)

	// Attempt loop: pre-first-token retry across backends (bounded).
	triedFirstByte := false
	var lastErr string
	tried := map[string]bool{}
	for attempt := 0; attempt <= g.maxRetries; attempt++ {
		decStart := time.Now()
		dec, derr := g.policy.SelectBackend(feat, snap)
		decLatency := ms(time.Since(decStart))
		if derr != nil {
			g.emit("ROUTE_ATTEMPT_FAILED", reqID, "", "", map[string]any{"reason": "no_eligible_backend", "attempt": attempt})
			httpErr(w, http.StatusServiceUnavailable, "NO_ELIGIBLE_BACKEND", reqID, "no healthy backend")
			return
		}
		// avoid re-picking a just-failed backend if alternatives exist
		if tried[dec.BackendID] {
			if alt := pickAlternative(snap, tried); alt != "" {
				dec.BackendID = alt
				dec.Reason += "+retry_alt"
			}
		}
		tried[dec.BackendID] = true
		routeID := fmt.Sprintf("%s.a%d", reqID, attempt)
		bs := g.backendByID(snap, dec.BackendID)
		if bs == nil {
			lastErr = "backend_not_in_snapshot"
			continue
		}
		g.emit("ROUTE_DECIDED", reqID, routeID, dec.BackendID, map[string]any{
			"policy": dec.PolicyName, "policy_version": dec.PolicyVersion, "reason": dec.Reason,
			"decision_latency_ms": decLatency, "attempt": attempt})
		g.emit("ROUTE_ATTEMPT_STARTED", reqID, routeID, dec.BackendID, nil)

		retryable, sent := g.forward(w, r, bs, body, reqID, routeID, stream, clientStart)
		if sent {
			triedFirstByte = true
			return // response delivered (or partial-stream failed, which is terminal)
		}
		// not sent => pre-first-byte failure; retry on another backend if allowed
		lastErr = retryable
		g.emit("ROUTE_ATTEMPT_FAILED", reqID, routeID, dec.BackendID, map[string]any{
			"reason": retryable, "attempt": attempt, "wasted_ms": ms(time.Since(decStart))})
		// refresh snapshot for next attempt
		snap = g.reg.Snapshot()
	}
	_ = triedFirstByte
	httpErr(w, http.StatusBadGateway, "ALL_ATTEMPTS_FAILED", reqID, lastErr)
}

// forward sends to one sidecar. Returns (retryReason, sentToClient). sentToClient=true means bytes
// were written to the client (no further cross-backend retry allowed). retryReason set when a
// pre-first-byte failure occurred (router may retry another backend).
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, bs *BackendState, body []byte,
	reqID, routeID string, stream bool, clientStart time.Time) (string, bool) {
	url := bs.Backend.SidecarURL + "/v1/chat/completions"
	req, _ := http.NewRequestWithContext(r.Context(), "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", reqID)
	req.Header.Set("X-Route-ID", routeID)
	req.Header.Set("X-Backend-ID", bs.Backend.ID)
	// Propagate the opaque experiment prefix headers to the sidecar so it can observe locality. The
	// sidecar STRIPS these before forwarding to vLLM (they never reach the model server). The raw key
	// is opaque experiment metadata, not prompt content.
	if v := r.Header.Get("X-Cache-Prefix-Key"); v != "" {
		req.Header.Set("X-Cache-Prefix-Key", v)
	}
	if v := r.Header.Get("X-Cache-Prefix-Tokens"); v != "" {
		req.Header.Set("X-Cache-Prefix-Tokens", v)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		// connection-level failure: no bytes to client -> retryable
		return "sidecar_connect_failed:" + err.Error(), false
	}
	defer resp.Body.Close()

	// Sidecar rejection (queue full / offline / draining) before any streaming -> retryable.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		return fmt.Sprintf("sidecar_reject_%d", resp.StatusCode), false
	}

	if !stream {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
		for k, vals := range resp.Header {
			if hopByHop[k] { continue }
			for _, v := range vals { w.Header().Add(k, v) }
		}
		w.Header().Set("X-Request-ID", reqID)
		w.Header().Set("X-Backend-ID", bs.Backend.ID)
		w.WriteHeader(resp.StatusCode)
		w.Write(out)
		g.emit("FIRST_CLIENT_BYTE", reqID, routeID, bs.Backend.ID, map[string]any{"ttft_ms": ms(time.Since(clientStart))})
		g.emit("REQUEST_COMPLETED", reqID, routeID, bs.Backend.ID, map[string]any{
			"status": resp.StatusCode, "stream": false, "e2e_ms": ms(time.Since(clientStart))})
		return "", true
	}

	// Streaming relay: forward each SSE event, flush immediately. After first byte: no reroute.
	flusher, ok := w.(http.Flusher)
	if !ok {
		return "no_flusher", false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Request-ID", reqID)
	w.Header().Set("X-Backend-ID", bs.Backend.ID)
	w.WriteHeader(http.StatusOK)

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var firstByte time.Time
	var events, bytesn int
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if firstByte.IsZero() {
				firstByte = time.Now()
				g.emit("FIRST_CLIENT_BYTE", reqID, routeID, bs.Backend.ID, map[string]any{"ttft_ms": ms(firstByte.Sub(clientStart))})
			}
			if _, werr := w.Write(line); werr != nil {
				g.emit("CLIENT_CANCELLED", reqID, routeID, bs.Backend.ID, map[string]any{"phase": "downstream_write_fail", "events": events})
				return "", true // client gone; terminal
			}
			flusher.Flush()
			events++
			bytesn += len(line)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			clientGone := r.Context().Err() != nil
			if firstByte.IsZero() {
				if clientGone {
					g.emit("CLIENT_CANCELLED", reqID, routeID, bs.Backend.ID, map[string]any{"phase": "pre_first_byte"})
					return "", true
				}
				// pre-first-byte stream error -> retryable
				return "stream_err_pre_first_byte:" + rerr.Error(), false
			}
			if clientGone {
				g.emit("CLIENT_CANCELLED", reqID, routeID, bs.Backend.ID, map[string]any{"phase": "mid_stream", "events": events})
				return "", true
			}
			// post-first-byte upstream failure: terminal, NO reroute
			g.emit("PARTIAL_STREAM_FAILED", reqID, routeID, bs.Backend.ID, map[string]any{
				"err": rerr.Error(), "events": events, "bytes": bytesn})
			return "", true
		}
		select {
		case <-r.Context().Done():
			g.emit("CLIENT_CANCELLED", reqID, routeID, bs.Backend.ID, map[string]any{"phase": "mid_stream", "events": events})
			return "", true
		default:
		}
	}
	g.emit("REQUEST_COMPLETED", reqID, routeID, bs.Backend.ID, map[string]any{
		"stream": true, "events": events, "bytes": bytesn, "e2e_ms": ms(time.Since(clientStart))})
	return "", true
}

func pickAlternative(snap *BackendSnapshot, tried map[string]bool) string {
	for _, b := range eligible(snap) {
		if !tried[b.Backend.ID] {
			return b.Backend.ID
		}
	}
	return ""
}

// --- helpers ---

var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Transfer-Encoding": true, "Upgrade": true,
	"Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailers": true,
}

func detectStream(body []byte) bool {
	var p struct{ Stream *bool `json:"stream"` }
	if json.Unmarshal(body, &p) == nil && p.Stream != nil {
		return *p.Stream
	}
	return false
}
func extractModel(body []byte) string {
	var p struct{ Model string `json:"model"` }
	json.Unmarshal(body, &p)
	return p.Model
}
func extractMaxTokens(body []byte) int {
	var p struct{ MaxTokens int `json:"max_tokens"` }
	json.Unmarshal(body, &p)
	return p.MaxTokens
}
func estimateTokens(body []byte) int {
	var p struct {
		Messages []struct{ Content string `json:"content"` } `json:"messages"`
	}
	if json.Unmarshal(body, &p) != nil {
		return 0
	}
	c := 0
	for _, m := range p.Messages {
		c += len(m.Content)
	}
	return c / 4
}
func orGen(v, prefix string) string {
	if len(v) > 0 && len(v) <= 128 {
		ok := true
		for _, c := range v {
			if !(c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
				ok = false
				break
			}
		}
		if ok {
			return v
		}
	}
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
func httpErr(w http.ResponseWriter, status int, code, reqID, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", reqID)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": detail, "request_id": reqID}})
}

var _ = context.Background

// emitCandidateState emits the full per-candidate analytical observation Liangqi's PPO needs to
// reconstruct any routing decision. For EVERY candidate backend it records the load/runtime/cache
// state AND the analytical cost breakdown (the base score over which PPO learns a residual). It
// computes scores via the cache-aware policy regardless of the active policy, so the RL state is
// always present even when routing with a baseline. No content, no raw keys.
func (g *Gateway) emitCandidateState(reqID string, feat RequestFeatures, snap *BackendSnapshot) {
	// Use the cache-aware analytical policy as the canonical base-score generator for RL state.
	scorer := NewCacheAwarePolicy(DefaultCacheAwareConfig(), g.reg)
	scores := scorer.ScoreBreakdown(feat, snap)
	byID := map[string]CandidateScore{}
	for _, s := range scores {
		byID[s.BackendID] = s
	}
	for i := range snap.Backends {
		b := &snap.Backends[i]
		cs, hasScore := byID[b.Backend.ID]
		fields := map[string]any{
			// load / runtime
			"queue_depth":      b.QueueDepth,
			"queue_inflight":   b.QueueInflight,
			"queue_max":        b.QueueMax,
			"runtime_running":  b.RuntimeRunning,
			"runtime_waiting":  b.RuntimeWaiting,
			"kv_cache_util":    b.KVCacheUtil,
			"kv_headroom":      b.KVHeadroom,
			"kv_headroom_supported": b.KVHeadroomSupported,
			"lifecycle_state":  b.LifecycleState,
			"stability_score":  b.StabilityScore,
			"snapshot_age_ms":  b.SnapshotAgeMs,
			// service rate (delta-derived, NOT cumulative total)
			"gen_tokens_per_sec":     b.GenTokensPerSec,
			"service_rate_supported": b.ServiceRateSupported,
			// cache observation
			"cache_observation_supported": b.CacheObservationSupported,
			"cache_match_supported":       b.CacheMatchSupported,
			"cache_confidence":            b.CacheConfidence,
			"cache_ready":                 b.CacheReady,
			"cache_snapshot_age_ms":       b.CacheSnapshotAgeMs,
			"cache_event_sequence":        b.CacheEventSequence,
			"cache_reset_epoch":           b.CacheResetEpoch,
			"cache_index_size":            b.CacheIndexSize,
			"eligible":                    isEligible(b),
		}
		if hasScore {
			fields["matched_prefix_tokens"] = cs.MatchedPrefixTokens
			fields["match_ratio"] = cs.MatchRatio
			fields["uncached_prompt_tokens"] = cs.UncachedPromptTokens
			fields["estimated_prefill_saved_ms"] = cs.EstPrefillSavedMs
			fields["est_queue_ms"] = cs.EstQueueMs
			fields["est_prefill_ms"] = cs.EstPrefillMs
			fields["est_decode_ms"] = cs.EstDecodeMs
			fields["cache_staleness_penalty_ms"] = cs.StalenessPenaltyMs
			fields["kv_pressure_penalty_ms"] = cs.KVPenaltyMs
			fields["final_analytical_score_ms"] = cs.FinalScoreMs
			fields["cache_used"] = cs.CacheUsed
		}
		g.emit("CANDIDATE_STATE", reqID, "", b.Backend.ID, fields)
	}
}

// isEligible mirrors the eligibility predicate for a single backend (for RL state honesty).
func isEligible(b *BackendState) bool {
	if !b.Reachable || !b.RuntimeHealthy {
		return false
	}
	if b.LifecycleState == "OFFLINE" || b.LifecycleState == "DRAINING" {
		return false
	}
	if b.QueueMax > 0 && b.QueueDepth >= b.QueueMax {
		return false
	}
	return true
}

// extractPrefixFeatures reads the opaque experiment prefix headers at the router and returns HASHED
// values (never raw). Router-side hashing matches the sidecar's (same SHA-256) so the directory key
// space agrees. The router does NOT strip these (the sidecar strips before forwarding to vLLM).
func extractPrefixFeatures(r *http.Request) (prefixKeyHash string, prefixTokens int, sessionKeyHash string) {
	if k := r.Header.Get("X-Cache-Prefix-Key"); k != "" {
		prefixKeyHash = hashRouterKey(k)
	}
	if t := r.Header.Get("X-Cache-Prefix-Tokens"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			if n > 1<<20 {
				n = 1 << 20
			}
			prefixTokens = n
		}
	}
	if s := r.Header.Get("X-Session-Key"); s != "" {
		sessionKeyHash = hashRouterKey(s)
	}
	return
}

func hashRouterKey(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
