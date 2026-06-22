package dataplane

import (
	"bufio"
	"errors"
	"bytes"
	"context"
	"encoding/json"
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
	HostID            string
	BackendID         string
	DeviceID          string
	MaxBufferBytes    int           // bounded per-request buffer for backpressure
	SlowConsumerWarnMs float64
	LogContent        bool          // MUST default false: never log prompts/responses
}

func DefaultProxyConfig() ProxyConfig {
	return ProxyConfig{MaxBufferBytes: 1 << 20, SlowConsumerWarnMs: 250, LogContent: false}
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
	cfg    ProxyConfig
	queue  *Queue
	vllm   *vllm.Adapter
	gate   Gate
	sink   EventSink

	// per-request bounded context counters (no full body retained)
	reqSeq atomic.Uint64
}

func NewProxy(cfg ProxyConfig, q *Queue, v *vllm.Adapter, gate Gate, sink EventSink) *Proxy {
	return &Proxy{cfg: cfg, queue: q, vllm: v, gate: gate, sink: sink}
}

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

	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "READ_BODY_FAILED", reqID, err.Error())
		return
	}
	// detect stream flag without full re-serialize
	stream := detectStream(body)

	p.emit("LOCAL_REQUEST_RECEIVED", reqID, routeID, map[string]any{
		"stream": stream, "body_bytes": len(body),
		"input_len_est": estimateInputTokens(body),
	})

	// Admit into bounded queue (lifecycle/health/drain gate inside Admit).
	tk, aerr := p.queue.Admit(r.Context(), reqID, routeID, p.cfg.BackendID, p.cfg.HostID, p.cfg.DeviceID, AdmissionGate(p.gate))
	if aerr != nil {
		p.emit("QUEUE_REJECTED", reqID, routeID, map[string]any{"reason": aerr.Error()})
		writeErr(w, rejectionStatus(aerr), aerr.Error(), reqID, "admission refused")
		return
	}
	p.emit("QUEUE_ENTERED", reqID, routeID, nil)

	if err := tk.WaitForDispatch(); err != nil {
		switch err {
		case ErrQueueTimeout:
			p.emit("QUEUE_TIMED_OUT", reqID, routeID, nil)
			writeErr(w, http.StatusServiceUnavailable, "QUEUE_TIMEOUT", reqID, "queued too long")
		case ErrCancelled:
			p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "in_queue"})
			// client gone; nothing to write
		default:
			writeErr(w, http.StatusServiceUnavailable, "DISPATCH_FAILED", reqID, err.Error())
		}
		return
	}
	p.emit("QUEUE_DEQUEUED", reqID, routeID, map[string]any{
		"queue_wait_ms": float64((tk.dispatchMono - tk.enqueuedMono).Microseconds()) / 1000.0,
	})

	// Dispatch to vLLM.
	tk.Transition(StateDispatching, "dispatch", p.queue.mono())
	p.emit("VLLM_DISPATCH_STARTED", reqID, routeID, nil)
	if stream {
		p.relayStream(w, r, tk, body, reqID, routeID)
	} else {
		p.relayJSON(w, r, tk, body, reqID, routeID)
	}
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
		p.queue.Done(tk, StateCompleted, "json_ok")
	} else {
		p.queue.Done(tk, StateUpstreamFail, "vllm_non_2xx")
	}
}

// relayStream implements the transparent SSE relay: read each upstream event and immediately
// write+flush it downstream. No full-answer buffering. Bounded read buffer for backpressure.
func (p *Proxy) relayStream(w http.ResponseWriter, r *http.Request, tk *Ticket, body []byte, reqID, routeID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "NO_FLUSH", reqID, "response writer not flushable")
		p.queue.Done(tk, StateUpstreamFail, "no_flusher")
		return
	}
	resp, connStart, err := p.upstreamRequest(tk.Context(), body, reqID, routeID)
	if err != nil {
		p.emit("VLLM_REQUEST_FAILED", reqID, routeID, map[string]any{"err": err.Error(), "phase": "connect"})
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
				p.emit("FIRST_VLLM_EVENT", reqID, routeID, map[string]any{
					"vllm_ttft_ms_from_dispatch": float64((p.queue.mono()-tk.dispatchMono).Microseconds())/1000.0})
			}
			// write+flush immediately (no buffering of the full answer)
			if _, werr := w.Write(line); werr != nil {
				// downstream (router/client) gone -> this is a CANCELLATION, not an upstream
				// partial failure. Cancel upstream and account it as cancelled.
				tk.cancel()
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
					p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "pre_first_event"})
					p.queue.Done(tk, StateCancelled, "cancelled_pre_first_event")
					return
				}
				// no bytes sent yet -> treat as upstream failure (router may retry)
				p.emit("VLLM_REQUEST_FAILED", reqID, routeID, map[string]any{"err": err.Error(), "phase": "pre_first_event"})
				p.queue.Done(tk, StateUpstreamFail, "upstream_err_pre_first_event")
				return
			}
			if cancelled {
				p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "mid_stream", "events": eventCount, "bytes": byteCount})
				p.queue.Done(tk, StateCancelled, "client_cancelled_mid_stream")
				return
			}
			p.emit("PARTIAL_STREAM_FAILED", reqID, routeID, map[string]any{
				"err": err.Error(), "events": eventCount, "bytes": byteCount})
			p.queue.Done(tk, StatePartialStream, "upstream_err_mid_stream")
			return
		}
		// honor client cancellation
		select {
		case <-tk.Context().Done():
			tk.cancel()
			p.emit("UPSTREAM_CANCELLED", reqID, routeID, map[string]any{"phase": "mid_stream"})
			p.queue.Done(tk, StateCancelled, "client_cancelled_mid_stream")
			return
		default:
		}
	}
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

var _ = strconv.Itoa
