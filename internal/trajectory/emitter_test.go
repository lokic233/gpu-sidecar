package trajectory

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmitter_BatchingAndDelivery(t *testing.T) {
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct{ Events []json.RawMessage `json:"events"` }
		json.Unmarshal(body, &p)
		received.Add(int64(len(p.Events)))
		w.WriteHeader(204)
	}))
	defer srv.Close()
	cfg := DefaultConfig(); cfg.CollectorURL = srv.URL + "/v1/events"; cfg.BatchSize = 10; cfg.FlushInterval = 50 * time.Millisecond
	e := New(cfg); e.Start(); defer e.Stop()
	for i := 0; i < 100; i++ {
		e.Emit(Event{Kind: "TEST", RequestID: "r"})
	}
	time.Sleep(300 * time.Millisecond)
	if received.Load() != 100 {
		t.Fatalf("want 100 events delivered, got %d", received.Load())
	}
}

func TestEmitter_CollectorOutageNonBlocking(t *testing.T) {
	cfg := DefaultConfig(); cfg.CollectorURL = "http://127.0.0.1:1/v1/events"; cfg.RequestTimeout = 100 * time.Millisecond
	e := New(cfg); e.Start(); defer e.Stop()
	// Emit must never block even though the collector is unreachable.
	start := time.Now()
	for i := 0; i < 1000; i++ {
		e.Emit(Event{Kind: "TEST", RequestID: "r"})
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Emit blocked under collector outage: %v", time.Since(start))
	}
}

func TestEmitter_QueueOverflowDrops(t *testing.T) {
	cfg := DefaultConfig(); cfg.CollectorURL = "http://127.0.0.1:1/v1/events"
	cfg.QueueCapacity = 10; cfg.RequestTimeout = 50 * time.Millisecond
	e := New(cfg)
	// don't Start() the loop, so the queue fills and overflows deterministically
	for i := 0; i < 200; i++ {
		e.Emit(Event{Kind: "NONTERMINAL", RequestID: "r"})
	}
	if e.Stats().Dropped == 0 {
		t.Fatal("expected dropped events on queue overflow")
	}
}

func TestEmitter_DisabledNoop(t *testing.T) {
	cfg := DefaultConfig(); cfg.Enabled = false
	e := New(cfg); e.Start(); defer e.Stop()
	e.Emit(Event{Kind: "TEST"})
	if e.Stats().Sent != 0 || e.Stats().Dropped != 0 {
		t.Fatal("disabled emitter should be a no-op")
	}
}

func TestEmitter_ConcurrentEmit(t *testing.T) {
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct{ Events []json.RawMessage `json:"events"` }
		json.Unmarshal(body, &p); received.Add(int64(len(p.Events))); w.WriteHeader(204)
	}))
	defer srv.Close()
	cfg := DefaultConfig(); cfg.CollectorURL = srv.URL + "/v1/events"; cfg.QueueCapacity = 100000
	e := New(cfg); e.Start(); defer e.Stop()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() { defer wg.Done(); for i := 0; i < 100; i++ { e.Emit(Event{Kind: "T", RequestID: "r"}) } }()
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond)
	if received.Load() == 0 {
		t.Fatal("concurrent emit delivered nothing")
	}
}
