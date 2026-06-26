package dataplane

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/runtime/vllm"
)

// fakeVLLM is a deterministic OpenAI-compatible server for integration tests.
type fakeVLLM struct {
	mu          sync.Mutex
	failConnect bool
	statusCode  int
	streamDelay time.Duration
	streamChunks int
	preFirstErr bool // close connection before any chunk
	srv         *httptest.Server
	cancelled   chan struct{}
	lastHeaders http.Header // headers seen on the last forwarded request (for strip tests)
}

func newFakeVLLM() *fakeVLLM {
	f := &fakeVLLM{statusCode: 200, streamChunks: 5, cancelled: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", f.chat)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vllm:num_requests_running 1\nvllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0.1\n"))
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeVLLM) chat(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	sc, chunks, delay, preErr := f.statusCode, f.streamChunks, f.streamDelay, f.preFirstErr
	f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.lastHeaders = r.Header.Clone()
	f.mu.Unlock()
	stream := strings.Contains(string(body), `"stream":true`) || strings.Contains(string(body), `"stream": true`)
	if sc != 200 {
		w.WriteHeader(sc)
		w.Write([]byte(`{"error":"fake"}`))
		return
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"completion_tokens":1}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	w.WriteHeader(200)
	if preErr {
		// simulate upstream dying before first event: hijack-close
		if hj, ok := w.(http.Hijacker); ok {
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
	}
	for i := 0; i < chunks; i++ {
		select {
		case <-r.Context().Done():
			f.cancelled <- struct{}{}
			return
		default:
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
		flusher.Flush()
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (f *fakeVLLM) close() { f.srv.Close() }

func newTestProxy(f *fakeVLLM, gate Gate) (*Proxy, *Queue, context.CancelFunc) {
	vcfg := vllm.DefaultConfig()
	vcfg.BaseURL = f.srv.URL
	v := vllm.New(vcfg)
	q := NewQueue(DefaultQueueConfig(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	pcfg := DefaultProxyConfig()
	pcfg.HostID = "test"; pcfg.BackendID = "test-b"; pcfg.DeviceID = "0"
	p := NewProxy(pcfg, q, v, gate, nil)
	return p, q, cancel
}

func TestProxy_NonStreamingJSON(t *testing.T) {
	f := newFakeVLLM(); defer f.close()
	p, _, cancel := newTestProxy(f, func() error { return nil }); defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200 got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hi") {
		t.Fatalf("missing content: %s", w.Body.String())
	}
}

func TestProxy_StreamingRelayAndDONE(t *testing.T) {
	f := newFakeVLLM(); defer f.close()
	p, _, cancel := newTestProxy(f, func() error { return nil }); defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	body := w.Body.String()
	count := strings.Count(body, "data:")
	if count < 5 {
		t.Fatalf("want >=5 SSE events, got %d: %s", count, body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("[DONE] must propagate: %s", body)
	}
}

func TestProxy_QueueFullRejection(t *testing.T) {
	f := newFakeVLLM(); defer f.close()
	cfg := DefaultQueueConfig()
	cfg.MaxQueuedRequests = 1
	cfg.MaxInflightRequests = 0 // nothing dispatches; queue fills
	vcfg := vllm.DefaultConfig(); vcfg.BaseURL = f.srv.URL
	q := NewQueue(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background()); defer cancel()
	go q.Run(ctx)
	pcfg := DefaultProxyConfig()
	p := NewProxy(pcfg, q, vllm.New(vcfg), func() error { return nil }, nil)
	// first request fills the queue (blocks in WaitForDispatch); run it in a goroutine
	go func() {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		p.ChatCompletions(httptest.NewRecorder(), r)
	}()
	time.Sleep(100 * time.Millisecond)
	// second request should be rejected 429
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	w2 := httptest.NewRecorder()
	p.ChatCompletions(w2, r2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("queue full should be 429, got %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "ADMISSION_QUEUE_FULL") {
		t.Fatalf("structured rejection expected: %s", w2.Body.String())
	}
}

func TestProxy_GateRejection(t *testing.T) {
	f := newFakeVLLM(); defer f.close()
	p, _, cancel := newTestProxy(f, func() error { return ErrBackendDraining }); defer cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	w := httptest.NewRecorder()
	p.ChatCompletions(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("drain gate should be 503, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "BACKEND_DRAINING") {
		t.Fatalf("expected BACKEND_DRAINING: %s", w.Body.String())
	}
}

func TestProxy_UpstreamNon200(t *testing.T) {
	f := newFakeVLLM(); f.statusCode = 500; defer f.close()
	p, _, cancel := newTestProxy(f, func() error { return nil }); defer cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	w := httptest.NewRecorder()
	p.ChatCompletions(w, r)
	if w.Code != 500 {
		t.Fatalf("upstream 500 should propagate, got %d", w.Code)
	}
}

func TestProxy_StreamEventsIncremental(t *testing.T) {
	f := newFakeVLLM(); f.streamChunks = 4; f.streamDelay = 30 * time.Millisecond; defer f.close()
	p, _, cancel := newTestProxy(f, func() error { return nil }); defer cancel()
	// use a pipe to observe flush timing
	pr, pw := io.Pipe()
	fw := &flushRecorder{w: pw, flushed: make(chan time.Time, 32)}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	go func() { p.ChatCompletions(fw, r); pw.Close() }()
	scanner := bufio.NewScanner(pr)
	var times []time.Time
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			times = append(times, time.Now())
		}
	}
	if len(times) < 4 {
		t.Fatalf("want >=4 incremental events, got %d", len(times))
	}
	// gap between first and last should reflect the per-chunk delay (not all-at-once)
	if times[len(times)-1].Sub(times[0]) < 40*time.Millisecond {
		t.Fatalf("events arrived too fast (buffered?): span=%v", times[len(times)-1].Sub(times[0]))
	}
}

// flushRecorder is an http.ResponseWriter+Flusher writing to a pipe.
type flushRecorder struct {
	w       io.Writer
	hdr     http.Header
	flushed chan time.Time
}

func (f *flushRecorder) Header() http.Header { if f.hdr == nil { f.hdr = http.Header{} }; return f.hdr }
func (f *flushRecorder) Write(b []byte) (int, error) { return f.w.Write(b) }
func (f *flushRecorder) WriteHeader(int) {}
func (f *flushRecorder) Flush() { select { case f.flushed <- time.Now(): default: } }
