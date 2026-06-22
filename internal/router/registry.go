// Package router implements the experimental Global Router Gateway: client-facing OpenAI-compatible
// endpoint, in-memory backend snapshot, deterministic backend selection, transparent relay, bounded
// pre-first-token retry, cancellation, and async trajectory emission. It NEVER scrapes GPU telemetry
// or vLLM metrics on the request hot path — it reads an already-materialized in-memory snapshot.
package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Backend is a configured GPU backend (a host sidecar's data-plane endpoint).
type Backend struct {
	ID          string `json:"id"`
	Vendor      string `json:"vendor"`
	SidecarURL  string `json:"sidecar_url"`  // e.g. http://[ipv6]:19095 (proxy /v1/chat/completions)
	SnapshotURL string `json:"snapshot_url"` // sidecar base for /v1/status,/v1/queue,/v1/runtime
}

// BackendSnapshot is the materialized, immutable view the policy reads (no hot-path scraping).
type BackendSnapshot struct {
	Backends  []BackendState `json:"backends"`
	Timestamp time.Time      `json:"timestamp"`
}

// BackendState is one backend's routing-facing state in the snapshot.
type BackendState struct {
	Backend           Backend `json:"backend"`
	Reachable         bool    `json:"reachable"`
	LifecycleState    string  `json:"lifecycle_state"`
	ControlPlaneReady bool    `json:"control_plane_ready"`
	StabilityScore    float64 `json:"stability_score"`
	HostCapacityHint  float64 `json:"host_capacity_hint"`
	// admission queue (host-level)
	QueueDepth   int `json:"queue_depth"`
	QueueInflight int `json:"queue_inflight"`
	QueueMax     int `json:"queue_max"`
	// vLLM runtime (DISTINCT runtime-level queue)
	RuntimeHealthy  bool    `json:"runtime_healthy"`
	RuntimeWaiting  float64 `json:"runtime_waiting"`
	RuntimeRunning  float64 `json:"runtime_running"`
	KVCacheUtil     float64 `json:"kv_cache_util"`
	SnapshotAgeMs   int64   `json:"snapshot_age_ms"`
}

// Registry materializes backend snapshots on a background loop. The hot path reads an atomic pointer.
type Registry struct {
	backends []Backend
	client   *http.Client
	snap     atomic.Pointer[BackendSnapshot]
	interval time.Duration
	stop     chan struct{}
	wg       sync.WaitGroup
}

func NewRegistry(backends []Backend, interval time.Duration) *Registry {
	r := &Registry{
		backends: backends, interval: interval, stop: make(chan struct{}),
		client: &http.Client{Timeout: 1 * time.Second},
	}
	empty := &BackendSnapshot{Timestamp: time.Now()}
	r.snap.Store(empty)
	return r
}

// Snapshot returns the latest materialized snapshot (hot-path safe; no I/O).
func (r *Registry) Snapshot() *BackendSnapshot { return r.snap.Load() }

func (r *Registry) Start() {
	r.wg.Add(1)
	go r.loop()
}
func (r *Registry) Stop() { close(r.stop); r.wg.Wait() }

func (r *Registry) loop() {
	defer r.wg.Done()
	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.refresh()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.refresh()
		}
	}
}

// refresh polls each backend's snapshot endpoints in parallel and builds an immutable snapshot.
func (r *Registry) refresh() {
	states := make([]BackendState, len(r.backends))
	var wg sync.WaitGroup
	for i, b := range r.backends {
		wg.Add(1)
		go func(i int, b Backend) {
			defer wg.Done()
			states[i] = r.pollBackend(b)
		}(i, b)
	}
	wg.Wait()
	r.snap.Store(&BackendSnapshot{Backends: states, Timestamp: time.Now()})
}

func (r *Registry) pollBackend(b Backend) BackendState {
	st := BackendState{Backend: b}
	// /readyz (control-plane) + first device lifecycle/stability/capacity
	if rz := r.getJSON(b.SnapshotURL + "/readyz"); rz != nil {
		st.Reachable = true
		st.ControlPlaneReady, _ = rz["control_plane_ready"].(bool)
	}
	if status := r.getJSON(b.SnapshotURL + "/v1/status"); status != nil {
		st.Reachable = true
		if devs, ok := status["devices"].([]any); ok && len(devs) > 0 {
			if d0, ok := devs[0].(map[string]any); ok {
				if ls, ok := d0["lifecycle_state"].(string); ok {
					st.LifecycleState = ls
				}
				if stab, ok := d0["stability"].(map[string]any); ok {
					st.StabilityScore, _ = stab["score"].(float64)
				}
				if cap, ok := d0["capacity"].(map[string]any); ok {
					st.HostCapacityHint, _ = cap["host_capacity_hint"].(float64)
				}
			}
		}
	}
	if q := r.getJSON(b.SnapshotURL + "/v1/queue"); q != nil {
		st.QueueDepth = intOf(q["queued_requests"])
		st.QueueInflight = intOf(q["inflight_requests"])
		st.QueueMax = intOf(q["max_queued_requests"])
	}
	if rt := r.getJSON(b.SnapshotURL + "/v1/runtime"); rt != nil {
		st.RuntimeHealthy, _ = rt["healthy"].(bool)
		st.RuntimeWaiting = fieldVal(rt["requests_waiting"])
		st.RuntimeRunning = fieldVal(rt["requests_running"])
		st.KVCacheUtil = fieldVal(rt["kv_cache_utilization"])
		if age, ok := rt["scrape_age_ms"].(float64); ok {
			st.SnapshotAgeMs = int64(age)
		}
	}
	return st
}

func (r *Registry) getJSON(url string) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	return m
}

func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// fieldVal extracts .value from a {value,supported} Field json object (or 0).
func fieldVal(v any) float64 {
	if m, ok := v.(map[string]any); ok {
		if f, ok := m["value"].(float64); ok {
			return f
		}
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}
