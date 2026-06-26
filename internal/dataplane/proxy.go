package dataplane

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/runtime/vllm"
)

// ProxyConfig configures the sidecar OpenAI-compatible proxy + relay.
type ProxyConfig struct {
	HostID             string
	BackendID          string
	DeviceID           string
	MaxBufferBytes     int     // bounded per-request buffer for backpressure
	SlowConsumerWarnMs float64
	LogContent         bool // MUST default false: never log prompts/responses

	// --- cache-aware experimental knobs (default off) ---
	// ExplicitHeaderEnabled enables reading X-Cache-Prefix-Key / X-Cache-Prefix-Tokens for the
	// deterministic explicit-prefix experiment mode. Disabled by default. When disabled the headers
	// are ignored AND still stripped before forwarding to vLLM.
	ExplicitHeaderEnabled bool
	// MaxPrefixTokens bounds the accepted X-Cache-Prefix-Tokens value (sanitization).
	MaxPrefixTokens int
}

func DefaultProxyConfig() ProxyConfig {
	return ProxyConfig{MaxBufferBytes: 1 << 20, SlowConsumerWarnMs: 250, LogContent: false,
		MaxPrefixTokens: 1 << 20}
}

// CacheObserver is the residency lifecycle hook the proxy uses to drive the per-prefix state machine
// (ABSENT->WARMING->READY). Implemented by the explicit cache provider; nil for non-cache sidecars.
// The proxy passes only a HASHED key — never the raw header value. A WARMING prefix is never a
// reusable hit, so a request that aborts before readiness leaves no false-positive cache entry.
type CacheObserver interface {
	// BeginWarm: this cache-eligible request was dispatched to the runtime (ABSENT->WARMING).
	BeginWarm(keyHash string, tokens int)
	// MarkReady: the warming request produced its first token / completed (WARMING->READY).
	MarkReady(keyHash string)
	// AbortWarm: the warming request failed before readiness (WARMING->ABSENT if last warmer).
	AbortWarm(keyHash string)
	// Lookup reports the current residency state + reusable READY token count for a key (no I/O).
	// Used at admission to size the work reservation conservatively.
	LookupState(keyHash string) (ready bool, readyTokens int)
}

// Gate decides whether the backend currently admits requests. Returns nil to admit.
type Gate func() error

// EventSink receives async local trajectory events (non-blocking).
type EventSink interface {
	Emit(ev LocalEvent)
}

// LocalEvent is a sidecar trajectory event.
type LocalEvent struct {
	Kind      string            `json:"kind"`
	RequestID string            `json:"request_id"`
	RouteID   string            `json:"route_id"`
	BackendID string            `json:"backend_id"`
	HostID    string            `json:"host_id"`
	DeviceID  string            `json:"device_id"`
	Wall      time.Time         `json:"wall"`
	Fields    map[string]any    `json:"fields,omitempty"`
}

// Proxy is the sidecar local data-plane HTTP handler.
type Proxy struct {
	cfg   ProxyConfig
	queue *Queue
	vllm  *vllm.Adapter
	gate  Gate
	sink  EventSink

	// cacheObs records explicit-prefix locality (nil when cache observation is off). Used only to
	// observe — never to gate or to store content.
	cacheObs CacheObserver

	// work optionally tracks token-level prefill/decode reservations (nil when off). Additive to the
	// hard request-count/inflight bounds; never replaces them.
	work *WorkAccountant

	// per-request bounded context counters (no full body retained)
	reqSeq atomic.Uint64
}

func NewProxy(cfg ProxyConfig, q *Queue, v *vllm.Adapter, gate Gate, sink EventSink) *Proxy {
	return &Proxy{cfg: cfg, queue: q, vllm: v, gate: gate, sink: sink}
}

// SetCacheObserver attaches the explicit-prefix cache observer (experimental mode). Safe to leave nil.
func (p *Proxy) SetCacheObserver(o CacheObserver) { p.cacheObs = o }

// SetWorkAccountant attaches the optional token-level work accountant. Safe to leave nil.
func (p *Proxy) SetWorkAccountant(w *WorkAccountant) { p.work = w }

func (p *Proxy) emit(kind, reqID, routeID string, fields map[string]any) {
	if p.sink == nil {
		return
	}
	p.sink.Emit(LocalEvent{
		Kind: kind, RequestID: reqID, RouteID: routeID, BackendID: p.cfg.BackendID,
		HostID: p.cfg.HostID, DeviceID: p.cfg.DeviceID, Wall: time.Now(), Fields: fields,
	})
}

// hopByHop headers stripped on relay.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailers": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

// ChatCompletions handles POST /v1/chat/completions: admit -> queue -> dispatch -> vLLM -> relay.
func (p *Proxy) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqID := orGenerate(r.Header.Get("X-Request-ID"), "req")
	routeID := orGenerate(r.Header.Get("X-Route-ID"), "rt")

	// Explicit-prefix experiment headers: read+HASH+STRIP. Disabled by default. The raw key is NEVER
	// logged, stored, or forwarded to vLLM. When disabled, headers are still stripped for hygiene.
	prefixKeyHash, prefixTokens := p.extractAndStripPrefixHeaders(r)

	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "READ_BODY_FAILED", reqID, err.Error())
		return
	}
	// detect stream flag without full re-serialize
	stream := detectStream(body)

	inputLen := estimateInputTokens(body)
	recvFields := map[string]any{
		"stream": stream, "body_bytes": len(body), "input_len_est": inputLen,
	}
	if prefixKeyHash != "" {
		// emit ONLY the hashed key + bounded token count (never the raw key)
		recvFields["cache_eligible"] = true
		recvFields["prefix_key_hash"] = prefixKeyHash
		recvFields["prefix_tokens"] = prefixTokens
	}
	p.emit("LOCAL_REQUEST_RECEIVED", reqID, routeID, recvFields)

	// Admit into bounded queue (lifecycle/health/drain gate inside Admit).
	tk, aerr := p.queue.Admit(r.Context(), reqID, routeID, p.cfg.BackendID, p.cfg.HostID, p.cfg.DeviceID, AdmissionGate(p.gate))
	if aerr != nil {
		p.emit("QUEUE_REJECTED", reqID, routeID, map[string]any{"reason": aerr.Error()})
		writeErr(w, rejectionStatus(aerr), aerr.Error(), reqID, "admission refused")
		return
	}
	p.emit("QUEUE_ENTERED", reqID, routeID, nil)

	// Reserve token work at ADMISSION (queued bucket), conservatively. Size from the LOCAL residency
	// state: only a READY (trustworthy) prefix lets us reserve on uncached tokens; ABSENT/WARMING/
	// unknown reserve on the FULL prompt. The reservation is carried on the ticket and released
	// exactly once on the terminal path. (Hard request-count/inflight bounds are unchanged.)
	if p.work != nil {
		readyTrust := false
		readyTokens := 0
		if p.cacheObs != nil && prefixKeyHash != "" {
			readyTrust, readyTokens = p.cacheObs.LookupState(prefixKeyHash)
		}
		tk.reservation = p.work.Reserve(inputLen, readyTokens, expectedOutputTokens(body), readyTrust)
	}
	tk.prefixKeyHash = prefixKeyHash
	tk.prefixTokens = prefixTokens
	if prefixKeyHash != "" && tk.prefixTokens <= 0 {
		tk.prefixTokens = inputLen
	}

	if err := tk.WaitForDispatch(); err != nil {
		switch err {
		case ErrQueueTimeout:
			p.resolveTerminal(tk, false) // queued reservation released; nothing was warming
			p.emit("QUEUE_TIMED_OUT", reqID, routeID, nil)
			writeErr(w, http.StatusServiceUnavailable, "QUEUE_TIMEOUT", reqID, "queued too long")
		case ErrCancelled:
			p.resolveTerminal(tk, false)
			p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "in_queue"})
			// client gone; nothing to write
		default:
			p.resolveTerminal(tk, false)
			writeErr(w, http.StatusServiceUnavailable, "DISPATCH_FAILED", reqID, err.Error())
		}
		return
	}
	p.emit("QUEUE_DEQUEUED", reqID, routeID, map[string]any{
		"queue_wait_ms": float64((tk.dispatchMono - tk.enqueuedMono).Microseconds()) / 1000.0,
	})

	// Begin WARMING at dispatch (ABSENT->WARMING): the request is now being served by the runtime,
	// but the prefix is NOT a reusable hit until it produces a first token / completes successfully.
	if p.cacheObs != nil && tk.prefixKeyHash != "" && tk.prefixTokens > 0 {
		p.cacheObs.BeginWarm(tk.prefixKeyHash, tk.prefixTokens)
		tk.warmBegun = true
	}

	// Dispatch to vLLM.
	tk.Transition(StateDispatching, "dispatch", p.queue.mono())
	p.emit("VLLM_DISPATCH_STARTED", reqID, routeID, nil)

	// Move the work reservation queued->active at dispatch.
	if tk.reservation != nil {
		tk.reservation.Activate()
	}

	if stream {
		p.relayStream(w, r, tk, body, reqID, routeID)
	} else {
		p.relayJSON(w, r, tk, body, reqID, routeID)
	}
}

// resolveTerminal runs the cache-residency + work-accounting terminal resolution EXACTLY ONCE for a
// ticket. ready=true promotes a warming prefix to READY (first token / 2xx); ready=false aborts the
// warm (pre-first-token failure / cancel / non-2xx / queue timeout). The work reservation is always
// released. Idempotent: safe to call from multiple terminal paths.
func (p *Proxy) resolveTerminal(tk *Ticket, ready bool) {
	tk.mu.Lock()
	if tk.resolved {
		tk.mu.Unlock()
		return
	}
	tk.resolved = true
	warmBegun := tk.warmBegun
	key := tk.prefixKeyHash
	res := tk.reservation
	tk.mu.Unlock()

	if p.cacheObs != nil && warmBegun && key != "" {
		if ready {
			p.cacheObs.MarkReady(key)
		} else {
			p.cacheObs.AbortWarm(key)
		}
	}
	if res != nil {
		res.Release()
	}
}

// expectedOutputTokens reads max_tokens from the body as the decode-work estimate (default 128).
func expectedOutputTokens(body []byte) int {
	var p struct {
		MaxTokens int `json:"max_tokens"`
	}
	if json.Unmarshal(body, &p) == nil && p.MaxTokens > 0 {
		return p.MaxTokens
	}
	return 128
}

// extractAndStripPrefixHeaders reads the opaque experiment prefix key + token count, returns the
// HASHED key (never raw) and a bounded token count, and ALWAYS removes the experimental headers from
// the request so they are never forwarded to vLLM. Returns ("", 0) when disabled or absent.
func (p *Proxy) extractAndStripPrefixHeaders(r *http.Request) (keyHash string, tokens int) {
	rawKey := r.Header.Get("X-Cache-Prefix-Key")
	rawTok := r.Header.Get("X-Cache-Prefix-Tokens")
	// strip unconditionally (hygiene): these are experimental and must not reach vLLM.
	r.Header.Del("X-Cache-Prefix-Key")
	r.Header.Del("X-Cache-Prefix-Tokens")
	if !p.cfg.ExplicitHeaderEnabled || rawKey == "" {
		return "", 0
	}
	keyHash = hashOpaqueKey(rawKey)
	if rawTok != "" {
		if n, err := strconv.Atoi(rawTok); err == nil && n > 0 {
			tokens = n
			if p.cfg.MaxPrefixTokens > 0 && tokens > p.cfg.MaxPrefixTokens {
				tokens = p.cfg.MaxPrefixTokens
			}
		}
	}
	return keyHash, tokens
}

// hashOpaqueKey returns a hex SHA-256 of the raw opaque key. The raw key NEVER leaves this function.
func hashOpaqueKey(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (p *Proxy) upstreamRequest(ctx context.Context, body []byte, reqID, routeID string) (*http.Response, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", p.vllm.ChatCompletionsURL(), bytes.NewReader(body))
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", reqID)
	req.Header.Set("X-Route-ID", routeID)
	connStart := time.Now()
	resp, err := p.vllm.ProxyClient.Do(req)
	return resp, connStart, err
}

func (p *Proxy) relayJSON(w http.ResponseWriter, r *http.Request, tk *Ticket, body []byte, reqID, routeID string) {
	tk.Transition(StateWaitingFull, "await_full", p.queue.mono())
	resp, connStart, err := p.upstreamRequest(tk.Context(), body, reqID, routeID)
	if err != nil {
		p.emit("VLLM_REQUEST_FAILED", reqID, routeID, map[string]any{"err": err.Error(), "phase": "connect"})
		p.resolveTerminal(tk, false) // pre-first-token failure -> abort warm, release work
		p.queue.Done(tk, StateUpstreamFail, "vllm_connect_failed")
		writeErr(w, http.StatusBadGateway, "UPSTREAM_FAILED", reqID, err.Error())
		return
	}
	defer resp.Body.Close()
	p.emit("VLLM_CONNECTED", reqID, routeID, map[string]any{"connect_ms": ms(time.Since(connStart))})
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	for k, vals := range resp.Header {
		if hopByHop[k] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Request-ID", reqID)
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	p.emit("STREAM_COMPLETED", reqID, routeID, map[string]any{"stream": false, "resp_bytes": len(out), "status": resp.StatusCode})
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		p.resolveTerminal(tk, true) // successful non-streaming completion -> WARMING->READY
		p.queue.Done(tk, StateCompleted, "json_ok")
	} else {
		p.resolveTerminal(tk, false) // runtime non-2xx -> abort warm
		p.queue.Done(tk, StateUpstreamFail, "vllm_non_2xx")
	}
}

// relayStream implements the transparent SSE relay: read each upstream event and immediately
// write+flush it downstream. No full-answer buffering. Bounded read buffer for backpressure.
func (p *Proxy) relayStream(w http.ResponseWriter, r *http.Request, tk *Ticket, body []byte, reqID, routeID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "NO_FLUSH", reqID, "response writer not flushable")
		p.resolveTerminal(tk, false)
		p.queue.Done(tk, StateUpstreamFail, "no_flusher")
		return
	}
	resp, connStart, err := p.upstreamRequest(tk.Context(), body, reqID, routeID)
	if err != nil {
		p.emit("VLLM_REQUEST_FAILED", reqID, routeID, map[string]any{"err": err.Error(), "phase": "connect"})
		p.resolveTerminal(tk, false) // pre-first-token failure -> abort warm
		p.queue.Done(tk, StateUpstreamFail, "vllm_connect_failed")
		writeErr(w, http.StatusBadGateway, "UPSTREAM_FAILED", reqID, err.Error())
		return
	}
	defer resp.Body.Close()
	p.emit("VLLM_CONNECTED", reqID, routeID, map[string]any{"connect_ms": ms(time.Since(connStart))})

	if resp.StatusCode != 200 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(out)
		p.resolveTerminal(tk, false) // non-200 -> abort warm
		p.queue.Done(tk, StateUpstreamFail, "vllm_non_200_stream")
		return
	}

	// SSE headers downstream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Request-ID", reqID)
	w.WriteHeader(http.StatusOK)
	tk.Transition(StateStreaming, "stream_open", p.queue.mono())

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var firstUpstream, firstDownstream time.Time
	var eventCount, byteCount int
	sawDone := false

	for {
		// read one SSE line (event). bufio handles chunking; we forward line-by-line.
		line, err := reader.ReadBytes('\n')
		now := time.Now()
		if len(line) > 0 {
			if firstUpstream.IsZero() {
				firstUpstream = now
				// FIRST valid upstream model event => WARMING->READY (prefix is now genuinely cached).
				p.resolveTerminal(tk, true)
				p.emit("FIRST_VLLM_EVENT", reqID, routeID, map[string]any{
					"vllm_ttft_ms_from_dispatch": float64((p.queue.mono()-tk.dispatchMono).Microseconds())/1000.0})
			}
			// write+flush immediately (no buffering of the full answer)
			if _, werr := w.Write(line); werr != nil {
				// downstream (router/client) gone -> this is a CANCELLATION. The prefix already
				// produced a token (READY); resolveTerminal(true) above already ran. Just release work.
				tk.cancel()
				p.resolveTerminal(tk, true)
				p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "downstream_write_fail"})
				p.queue.Done(tk, StateCancelled, "downstream_gone")
				return
			}
			flusher.Flush()
			if firstDownstream.IsZero() {
				firstDownstream = now
				p.emit("FIRST_RELAY_WRITE", reqID, routeID, map[string]any{
					"relay_delay_ms": ms(firstDownstream.Sub(firstUpstream))})
			}
			eventCount++
			byteCount += len(line)
			if bytes.Contains(line, []byte("[DONE]")) {
				sawDone = true
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// Distinguish client/router cancellation (ctx canceled) from a true upstream failure.
			cancelled := tk.Context().Err() != nil || errors.Is(err, context.Canceled)
			if firstDownstream.IsZero() {
				if cancelled {
					p.resolveTerminal(tk, false) // cancelled before any token -> abort warm
					p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "pre_first_event"})
					p.queue.Done(tk, StateCancelled, "cancelled_pre_first_event")
					return
				}
				// no bytes sent yet -> treat as upstream failure (router may retry)
				p.resolveTerminal(tk, false) // pre-first-token failure -> abort warm
				p.emit("VLLM_REQUEST_FAILED", reqID, routeID, map[string]any{"err": err.Error(), "phase": "pre_first_event"})
				p.queue.Done(tk, StateUpstreamFail, "upstream_err_pre_first_event")
				return
			}
			if cancelled {
				p.resolveTerminal(tk, true) // already produced a token (READY); release work
				p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "mid_stream", "events": eventCount, "bytes": byteCount})
				p.queue.Done(tk, StateCancelled, "client_cancelled_mid_stream")
				return
			}
			p.resolveTerminal(tk, true) // partial stream AFTER first token -> prefix was cached; release work
			p.emit("PARTIAL_STREAM_FAILED", reqID, routeID, map[string]any{
				"err": err.Error(), "events": eventCount, "bytes": byteCount})
			p.queue.Done(tk, StatePartialStream, "upstream_err_mid_stream")
			return
		}
		// honor client cancellation
		select {
		case <-tk.Context().Done():
			tk.cancel()
			// if a token already arrived, firstUpstream is set and resolveTerminal(true) already ran;
			// otherwise this is a pre-first-token cancel.
			p.resolveTerminal(tk, !firstUpstream.IsZero())
			p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "mid_stream"})
			p.queue.Done(tk, StateCancelled, "client_cancelled_mid_stream")
			return
		default:
		}
	}
	p.resolveTerminal(tk, !firstUpstream.IsZero()) // clean completion: ready iff a token was produced
	p.emit("STREAM_COMPLETED", reqID, routeID, map[string]any{
		"stream": true, "events": eventCount, "bytes": byteCount, "saw_done": sawDone})
	p.queue.Done(tk, StateCompleted, "stream_ok")
}

// --- helpers ---

func detectStream(body []byte) bool {
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Stream != nil {
		return *probe.Stream
	}
	return false
}

func estimateInputTokens(body []byte) int {
	// rough estimate: chars/4 over message contents (no content stored)
	var probe struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return 0
	}
	chars := len(probe.Prompt)
	for _, m := range probe.Messages {
		chars += len(m.Content)
	}
	return chars / 4
}

func orGenerate(v, prefix string) string {
	v = sanitizeID(v)
	if v != "" {
		return v
	}
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

// sanitizeID validates an externally-supplied correlation id: bounded length, safe chars only.
func sanitizeID(v string) string {
	if len(v) == 0 || len(v) > 128 {
		return ""
	}
	for _, c := range v {
		if !(c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return ""
		}
	}
	return v
}

func rejectionStatus(err error) int {
	switch err {
	case ErrQueueFull, ErrInflightFull:
		return http.StatusTooManyRequests // 429
	case ErrBackendOffline, ErrBackendDraining, ErrRuntimeUnhealthy:
		return http.StatusServiceUnavailable // 503
	case ErrQueueTimeout:
		return http.StatusServiceUnavailable
	default:
		return http.StatusServiceUnavailable
	}
}

func writeErr(w http.ResponseWriter, status int, code, reqID, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", reqID)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": detail, "request_id": reqID},
	})
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
