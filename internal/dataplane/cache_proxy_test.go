package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/runtime/vllm"
)

// recordObserver captures residency-lifecycle calls and models a tiny READY set for LookupState.
type recordObserver struct {
	mu        sync.Mutex
	begun     []string
	begunTok  []int
	readied   []string
	aborted   []string
	ready     map[string]int // keyHash -> ready tokens (for LookupState)
}

func newRecordObserver() *recordObserver { return &recordObserver{ready: map[string]int{}} }

func (r *recordObserver) BeginWarm(keyHash string, tokens int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.begun = append(r.begun, keyHash)
	r.begunTok = append(r.begunTok, tokens)
}
func (r *recordObserver) MarkReady(keyHash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readied = append(r.readied, keyHash)
	r.ready[keyHash] = 1
}
func (r *recordObserver) AbortWarm(keyHash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aborted = append(r.aborted, keyHash)
}
func (r *recordObserver) LookupState(keyHash string) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok, ok := r.ready[keyHash]
	return ok, tok
}
func (r *recordObserver) counts() (begun, readied, aborted int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.begun), len(r.readied), len(r.aborted)
}

func newCacheProxy(f *fakeVLLM, explicitOn bool) (*Proxy, *recordObserver, *WorkAccountant, context.CancelFunc) {
	vcfg := vllm.DefaultConfig()
	vcfg.BaseURL = f.srv.URL
	v := vllm.New(vcfg)
	q := NewQueue(DefaultQueueConfig(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	pcfg := DefaultProxyConfig()
	pcfg.HostID = "test"
	pcfg.BackendID = "test-b"
	pcfg.DeviceID = "0"
	pcfg.ExplicitHeaderEnabled = explicitOn
	p := NewProxy(pcfg, q, v, func() error { return nil }, nil)
	obs := newRecordObserver()
	p.SetCacheObserver(obs)
	wa := NewWorkAccountant()
	p.SetWorkAccountant(wa)
	return p, obs, wa, cancel
}

func sha(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func TestProxy_ExplicitHeaderStrippedAndHashed(t *testing.T) {
	f := newFakeVLLM()
	defer f.close()
	p, obs, _, cancel := newCacheProxy(f, true)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Cache-Prefix-Key", "super-secret-prefix")
	req.Header.Set("X-Cache-Prefix-Tokens", "128")
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200 got %d", w.Code)
	}
	// the experimental headers MUST be stripped before reaching vLLM
	f.mu.Lock()
	h := f.lastHeaders
	f.mu.Unlock()
	if h.Get("X-Cache-Prefix-Key") != "" || h.Get("X-Cache-Prefix-Tokens") != "" {
		t.Fatalf("experimental headers must be stripped before forwarding")
	}
	// BeginWarm must receive the HASHED key, never the raw one
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.begun) != 1 {
		t.Fatalf("expected 1 BeginWarm, got %d", len(obs.begun))
	}
	if obs.begun[0] == "super-secret-prefix" {
		t.Fatalf("observer must NOT receive the raw key")
	}
	if obs.begun[0] != sha("super-secret-prefix") {
		t.Fatalf("observer must receive the sha256 of the key")
	}
	if obs.begunTok[0] != 128 {
		t.Fatalf("expected 128 prefix tokens, got %d", obs.begunTok[0])
	}
	// a successful non-streaming completion -> MarkReady (WARMING->READY)
	if len(obs.readied) != 1 || obs.readied[0] != sha("super-secret-prefix") {
		t.Fatalf("expected MarkReady on success, got %v", obs.readied)
	}
}

func TestProxy_PreFirstTokenFailureAbortsWarm(t *testing.T) {
	f := newFakeVLLM()
	f.statusCode = 500 // runtime non-2xx -> pre-first-token failure
	defer f.close()
	p, obs, _, cancel := newCacheProxy(f, true)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Cache-Prefix-Key", "k")
	req.Header.Set("X-Cache-Prefix-Tokens", "16")
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	begun, readied, aborted := obs.counts()
	if begun != 1 {
		t.Fatalf("expected BeginWarm, got %d", begun)
	}
	if readied != 0 {
		t.Fatalf("a failed request must NOT MarkReady, got %d", readied)
	}
	if aborted != 1 {
		t.Fatalf("a pre-first-token failure must AbortWarm, got %d", aborted)
	}
}

func TestProxy_ExplicitHeaderDisabled_StillStripped(t *testing.T) {
	f := newFakeVLLM()
	defer f.close()
	p, obs, _, cancel := newCacheProxy(f, false)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Cache-Prefix-Key", "secret")
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	f.mu.Lock()
	h := f.lastHeaders
	f.mu.Unlock()
	if h.Get("X-Cache-Prefix-Key") != "" {
		t.Fatalf("header must be stripped even when explicit mode disabled")
	}
	if begun, _, _ := obs.counts(); begun != 0 {
		t.Fatalf("disabled explicit mode must not warm, got %d", begun)
	}
}

func TestProxy_NoCacheHeader_NoBehaviorChange(t *testing.T) {
	f := newFakeVLLM()
	defer f.close()
	p, obs, _, cancel := newCacheProxy(f, true)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200 got %d", w.Code)
	}
	if begun, _, _ := obs.counts(); begun != 0 {
		t.Fatalf("absent header must not warm")
	}
}

func TestProxy_WorkReservationLifecycle(t *testing.T) {
	f := newFakeVLLM()
	defer f.close()
	p, _, wa, cancel := newCacheProxy(f, true)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hello there"}]}`))
	req.Header.Set("X-Cache-Prefix-Key", "k")
	req.Header.Set("X-Cache-Prefix-Tokens", "4")
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200 got %d", w.Code)
	}
	// after completion, ALL outstanding work returns to 0; lifetime totals advanced; never negative.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		ws := wa.Snapshot()
		if ws.TotalOutstandingPrefill == 0 && ws.TotalOutstandingDecode == 0 &&
			ws.QueuedReservedDecodeTokens == 0 && ws.ActiveDecodeTokens == 0 {
			if ws.LifetimeReservedDecode == 0 {
				t.Fatalf("expected decode work to have been booked")
			}
			// invariant: outstanding = queued + active (both 0 here)
			if ws.TotalOutstandingPrefill != ws.QueuedReservedPrefillTokens+ws.ActivePrefillTokens {
				t.Fatalf("outstanding != queued+active")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reservations not released after completion: %+v", wa.Snapshot())
}

func TestWorkAccountant_Invariants(t *testing.T) {
	wa := NewWorkAccountant()
	// reserve at admission (queued), activate (move to active), release (back to 0).
	r := wa.Reserve(100, 0, 50, false) // unknown cache -> full prompt reserved
	s := wa.Snapshot()
	if s.QueuedReservedPrefillTokens != 100 || s.QueuedReservedDecodeTokens != 50 {
		t.Fatalf("expected queued 100/50, got %+v", s)
	}
	if s.TotalOutstandingPrefill != 100 || s.ActivePrefillTokens != 0 {
		t.Fatalf("outstanding/active wrong: %+v", s)
	}
	r.Activate()
	s = wa.Snapshot()
	if s.QueuedReservedPrefillTokens != 0 || s.ActivePrefillTokens != 100 {
		t.Fatalf("activate must move queued->active, got %+v", s)
	}
	if s.TotalOutstandingPrefill != 100 {
		t.Fatalf("outstanding must stay 100 across activate, got %d", s.TotalOutstandingPrefill)
	}
	r.Release()
	s = wa.Snapshot()
	if s.TotalOutstandingPrefill != 0 || s.TotalOutstandingDecode != 0 {
		t.Fatalf("release must zero outstanding, got %+v", s)
	}
	r.Release() // idempotent
	if wa.Snapshot().TotalOutstandingPrefill != 0 {
		t.Fatalf("double release must not go negative")
	}
}

func TestWorkAccountant_ReadyReservesUncached(t *testing.T) {
	wa := NewWorkAccountant()
	// READY trustworthy match of 80 of 100 input tokens -> reserve only 20 uncached prefill.
	r := wa.Reserve(100, 80, 50, true)
	if s := wa.Snapshot(); s.QueuedReservedPrefillTokens != 20 {
		t.Fatalf("READY match should reserve uncached (20), got %d", s.QueuedReservedPrefillTokens)
	}
	r.Release()
	// low-trust same numbers -> reserve FULL prompt (100).
	r2 := wa.Reserve(100, 80, 50, false)
	if s := wa.Snapshot(); s.QueuedReservedPrefillTokens != 100 {
		t.Fatalf("untrusted match must reserve full prompt (100), got %d", s.QueuedReservedPrefillTokens)
	}
	r2.Release()
}

func TestWorkAccountant_ConcurrentReleaseRaceClean(t *testing.T) {
	wa := NewWorkAccountant()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := wa.Reserve(50, 0, 20, false)
			r.Activate()
			r.Release()
			r.Release() // idempotent under concurrency
		}()
	}
	wg.Wait()
	s := wa.Snapshot()
	if s.TotalOutstandingPrefill != 0 || s.TotalOutstandingDecode != 0 {
		t.Fatalf("all reservations must net to 0, got %+v", s)
	}
}
