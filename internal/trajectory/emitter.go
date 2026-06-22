// Package trajectory provides a non-blocking, bounded, batched event emitter that ships events to
// a configurable Response/Trajectory Collector. Collector failure or slowness MUST NOT block the
// request/streaming path. When the bounded queue is full, events are dropped (counter incremented),
// preferring to preserve terminal events. Optional bounded JSONL local fallback.
package trajectory

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures the emitter.
type Config struct {
	Enabled         bool
	CollectorURL    string        // e.g. http://127.0.0.1:9100/v1/events  (configurable by URL)
	BatchSize       int
	FlushInterval   time.Duration
	QueueCapacity   int
	RequestTimeout  time.Duration
	LocalFallbackPath string      // bounded JSONL fallback ("" = disabled)
	Source          string        // "router" | "sidecar:<host>"
}

func DefaultConfig() Config {
	return Config{
		Enabled: true, CollectorURL: "http://127.0.0.1:9100/v1/events",
		BatchSize: 128, FlushInterval: 500 * time.Millisecond, QueueCapacity: 10000,
		RequestTimeout: 500 * time.Millisecond, LocalFallbackPath: "",
	}
}

// Event is a generic trajectory event (router or sidecar). Kept small; NO prompt/response content.
type Event struct {
	Kind      string         `json:"kind"`
	Source    string         `json:"source"`
	RequestID string         `json:"request_id"`
	RouteID   string         `json:"route_id,omitempty"`
	BackendID string         `json:"backend_id,omitempty"`
	HostID    string         `json:"host_id,omitempty"`
	DeviceID  string         `json:"device_id,omitempty"`
	Wall      time.Time      `json:"wall"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// terminal event kinds are preserved under pressure when practical.
var terminalKinds = map[string]bool{
	"REQUEST_COMPLETED": true, "PARTIAL_STREAM_FAILED": true, "ROUTE_ATTEMPT_FAILED": true,
	"CLIENT_CANCELLED": true, "STREAM_COMPLETED": true, "QUEUE_REJECTED": true,
	"QUEUE_TIMED_OUT": true, "VLLM_REQUEST_FAILED": true, "UPSTREAM_CANCELLED": true,
}

// Emitter ships events asynchronously.
type Emitter struct {
	cfg    Config
	ch     chan Event
	client *http.Client
	stop   chan struct{}
	wg     sync.WaitGroup

	dropped  atomic.Uint64
	sent     atomic.Uint64
	failed   atomic.Uint64
	fallback atomic.Uint64

	fbMu   sync.Mutex
	fbFile *os.File
	fbBytes int64
}

func New(cfg Config) *Emitter {
	if cfg.BatchSize <= 0 { cfg.BatchSize = 128 }
	if cfg.QueueCapacity <= 0 { cfg.QueueCapacity = 10000 }
	e := &Emitter{
		cfg: cfg, ch: make(chan Event, cfg.QueueCapacity),
		client: &http.Client{Timeout: cfg.RequestTimeout},
		stop: make(chan struct{}),
	}
	if cfg.LocalFallbackPath != "" {
		if f, err := os.OpenFile(cfg.LocalFallbackPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			e.fbFile = f
		}
	}
	return e
}

// Emit enqueues an event WITHOUT blocking. If the queue is full, the event is dropped (counter++),
// except terminal events which get one short bounded retry to preserve them.
func (e *Emitter) Emit(ev Event) {
	if !e.cfg.Enabled {
		return
	}
	if ev.Source == "" {
		ev.Source = e.cfg.Source
	}
	if ev.Wall.IsZero() {
		ev.Wall = time.Now()
	}
	select {
	case e.ch <- ev:
	default:
		// queue full: preserve terminal events with a tiny non-blocking-ish attempt
		if terminalKinds[ev.Kind] {
			select {
			case e.ch <- ev:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
		e.dropped.Add(1)
	}
}

func (e *Emitter) Start() {
	if !e.cfg.Enabled {
		return
	}
	e.wg.Add(1)
	go e.loop()
}

func (e *Emitter) Stop() {
	if !e.cfg.Enabled {
		return
	}
	close(e.stop)
	e.wg.Wait()
	if e.fbFile != nil {
		e.fbFile.Close()
	}
}

func (e *Emitter) loop() {
	defer e.wg.Done()
	t := time.NewTicker(e.cfg.FlushInterval)
	defer t.Stop()
	batch := make([]Event, 0, e.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		e.ship(batch)
		batch = batch[:0]
	}
	for {
		select {
		case <-e.stop:
			// drain quickly
			for {
				select {
				case ev := <-e.ch:
					batch = append(batch, ev)
					if len(batch) >= e.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case ev := <-e.ch:
			batch = append(batch, ev)
			if len(batch) >= e.cfg.BatchSize {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// ship posts a batch with bounded backoff (one retry). On failure, optionally write JSONL fallback.
func (e *Emitter) ship(batch []Event) {
	payload, err := json.Marshal(map[string]any{"events": batch})
	if err != nil {
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), e.cfg.RequestTimeout)
		req, _ := http.NewRequestWithContext(ctx, "POST", e.cfg.CollectorURL, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, derr := e.client.Do(req)
		cancel()
		if derr == nil && resp != nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				e.sent.Add(uint64(len(batch)))
				return
			}
		}
		if attempt == 0 {
			time.Sleep(50 * time.Millisecond) // bounded backoff
		}
	}
	e.failed.Add(uint64(len(batch)))
	e.writeFallback(batch)
}

func (e *Emitter) writeFallback(batch []Event) {
	if e.fbFile == nil {
		return
	}
	e.fbMu.Lock()
	defer e.fbMu.Unlock()
	const maxFallback = 64 * 1024 * 1024 // bounded fallback file
	if e.fbBytes > maxFallback {
		return
	}
	for _, ev := range batch {
		b, _ := json.Marshal(ev)
		b = append(b, '\n')
		n, _ := e.fbFile.Write(b)
		e.fbBytes += int64(n)
		e.fallback.Add(1)
	}
}

// Stats returns emitter counters for observability.
type Stats struct {
	Sent     uint64 `json:"sent"`
	Dropped  uint64 `json:"dropped"`
	Failed   uint64 `json:"failed"`
	Fallback uint64 `json:"fallback_written"`
	QueueLen int    `json:"queue_len"`
	QueueCap int    `json:"queue_cap"`
}

func (e *Emitter) Stats() Stats {
	return Stats{
		Sent: e.sent.Load(), Dropped: e.dropped.Load(), Failed: e.failed.Load(),
		Fallback: e.fallback.Load(), QueueLen: len(e.ch), QueueCap: cap(e.ch),
	}
}
