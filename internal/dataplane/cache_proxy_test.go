package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/runtime/vllm"
)

// recordObserver captures Observe calls for assertions.
type recordObserver struct {
	keys   []string
	tokens []int
}

func (r *recordObserver) Observe(keyHash string, tokens int) {
	r.keys = append(r.keys, keyHash)
	r.tokens = append(r.tokens, tokens)
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
	obs := &recordObserver{}
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
		t.Fatalf("experimental headers must be stripped before forwarding, got key=%q tok=%q",
			h.Get("X-Cache-Prefix-Key"), h.Get("X-Cache-Prefix-Tokens"))
	}
	// observer must receive the HASHED key, never the raw one
	if len(obs.keys) != 1 {
		t.Fatalf("expected 1 observe call, got %d", len(obs.keys))
	}
	if obs.keys[0] == "super-secret-prefix" {
		t.Fatalf("observer must NOT receive the raw key")
	}
	if obs.keys[0] != sha("super-secret-prefix") {
		t.Fatalf("observer must receive the sha256 of the key")
	}
	if obs.tokens[0] != 128 {
		t.Fatalf("expected 128 prefix tokens observed, got %d", obs.tokens[0])
	}
}

func TestProxy_ExplicitHeaderDisabled_StillStripped(t *testing.T) {
	f := newFakeVLLM()
	defer f.close()
	p, obs, _, cancel := newCacheProxy(f, false) // explicit DISABLED
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Cache-Prefix-Key", "secret")
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	// headers still stripped for hygiene
	f.mu.Lock()
	h := f.lastHeaders
	f.mu.Unlock()
	if h.Get("X-Cache-Prefix-Key") != "" {
		t.Fatalf("header must be stripped even when explicit mode disabled")
	}
	// but no observation occurs (mode off => no behavior change)
	if len(obs.keys) != 0 {
		t.Fatalf("disabled explicit mode must not observe, got %d", len(obs.keys))
	}
}

func TestProxy_NoCacheHeader_NoBehaviorChange(t *testing.T) {
	f := newFakeVLLM()
	defer f.close()
	p, obs, _, cancel := newCacheProxy(f, true)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	// no cache headers at all
	w := httptest.NewRecorder()
	p.ChatCompletions(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200 got %d", w.Code)
	}
	if len(obs.keys) != 0 {
		t.Fatalf("absent header must not trigger observation")
	}
}

func TestProxy_WorkAccountingReleasedAfterComplete(t *testing.T) {
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
	// after completion, reservations must be released back to 0 (deferred Release ran).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		ws := wa.Snapshot()
		if ws.ReservedDecodeTokens == 0 && ws.ReservedUncachedPrefillTokens == 0 &&
			ws.ActiveDecodeTokens == 0 && ws.ActiveUncachedPrefillTokens == 0 {
			// but the monotonic totals must have advanced (work WAS booked)
			if ws.TotalReservedDecodeTokens == 0 {
				t.Fatalf("expected decode work to have been booked")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reservations not released after completion: %+v", wa.Snapshot())
}
