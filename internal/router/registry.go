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
	QueueDepth    int `json:"queue_depth"`
	QueueInflight int `json:"queue_inflight"`
	QueueMax      int `json:"queue_max"`
	// vLLM runtime (DISTINCT runtime-level queue)
	RuntimeHealthy bool    `json:"runtime_healthy"`
	RuntimeWaiting float64 `json:"runtime_waiting"`
	RuntimeRunning float64 `json:"runtime_running"`
	KVCacheUtil    float64 `json:"kv_cache_util"`
	SnapshotAgeMs  int64   `json:"snapshot_age_ms"`

	// --- measured service rate (derived from cumulative counters via deltas; NOT raw totals) ---
	// GenTokensPerSec is the measured generation throughput (tokens/s) computed by differencing the
	// cumulative vllm:generation_tokens_total counter over wall time between snapshots. 0 + Supported
	// =false until two consecutive scrapes exist.
	GenTokensPerSec       float64 `json:"gen_tokens_per_sec"`
	ServiceRateSupported  bool    `json:"service_rate_supported"`

	// --- cache-locality state (materialized off the hot path from sidecar GET /v1/cache) ---
	CacheObservationSupported bool    `json:"cache_observation_supported"`
	CacheMatchSupported       bool    `json:"cache_match_supported"`
	CacheReady                bool    `json:"cache_ready"`
	CacheConfidence           float64 `json:"cache_confidence"`
	CacheSnapshotAgeMs        int64   `json:"cache_snapshot_age_ms"`
	KVHeadroom                float64 `json:"kv_headroom"`           // [0,1]; 1-kv_util when known
	KVHeadroomSupported       bool    `json:"kv_headroom_supported"`
	CacheEventSequence        int64   `json:"cache_event_sequence"`
	CacheIndexSize            int     `json:"cache_index_size"`
	CacheResetEpoch           int64   `json:"cache_reset_epoch"`
	CacheProvider             string  `json:"cache_provider"`

	// PrefixMatchedTokens is the per-request matched prefix length for the CURRENT request, filled
	// in by the policy via the local cache directory (see CacheDirectory). Not part of the polled
	// snapshot; populated transiently during a routing decision when explicit mode is used.
	PrefixMatchedTokens int `json:"-"`
}

// Registry materializes backend snapshots on a background loop. The hot path reads an atomic pointer.
type Registry struct {
	backends []Backend
	client   *http.Client
	snap     atomic.Pointer[BackendSnapshot]
	interval time.Duration
	stop     chan struct{}
	wg       sync.WaitGroup

	// cacheDir holds a per-backend bounded directory of opaque prefix-key -> matched tokens,
	// materialized OFF the hot path. The policy reads it with an O(1) local map lookup per request
	// (no per-request network query). Guarded by dirMu; swapped atomically per refresh.
	dirMu        sync.RWMutex
	cacheDir     map[string]map[string]int // backendID -> (prefixKeyHash -> matchedTokens)
	cacheDirMax  int

	// prev holds the previous scrape's cumulative counters per backend for service-rate deltas.
	prevMu sync.Mutex
	prev   map[string]counterSample
}

// counterSample stores cumulative counters + wall time for rate differencing.
type counterSample struct {
	genTokensTotal float64
	supported      bool
	at             time.Time
}

func NewRegistry(backends []Backend, interval time.Duration) *Registry {
	r := &Registry{
		backends: backends, interval: interval, stop: make(chan struct{}),
		client:      &http.Client{Timeout: 1 * time.Second},
		cacheDir:    map[string]map[string]int{},
		cacheDirMax: 4096,
		prev:        map[string]counterSample{},
	}
	empty := &BackendSnapshot{Timestamp: time.Now()}
	r.snap.Store(empty)
	return r
}

// LookupPrefixTokens returns the matched prefix-token count for a backend+prefix key from the
// materialized cache directory (O(1) local lookup; NO network I/O). 0 when not present/unknown.
func (r *Registry) LookupPrefixTokens(backendID, prefixKeyHash string) int {
	if prefixKeyHash == "" {
		return 0
	}
	r.dirMu.RLock()
	defer r.dirMu.RUnlock()
	if d, ok := r.cacheDir[backendID]; ok {
		return d[prefixKeyHash]
	}
	return 0
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
	dirs := make([]map[string]int, len(r.backends))
	var wg sync.WaitGroup
	for i, b := range r.backends {
		wg.Add(1)
		go func(i int, b Backend) {
			defer wg.Done()
			states[i], dirs[i] = r.pollBackend(b)
		}(i, b)
	}
	wg.Wait()
	r.snap.Store(&BackendSnapshot{Backends: states, Timestamp: time.Now()})
	// swap the materialized cache directory atomically (off the hot path).
	newDir := make(map[string]map[string]int, len(r.backends))
	for i, b := range r.backends {
		if dirs[i] != nil {
			newDir[b.ID] = dirs[i]
		}
	}
	r.dirMu.Lock()
	r.cacheDir = newDir
	r.dirMu.Unlock()
}

func (r *Registry) pollBackend(b Backend) (BackendState, map[string]int) {
	st := BackendState{Backend: b}
	var dir map[string]int
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
		// Service rate: difference the cumulative generation_tokens_total counter over wall time.
		// generation_tokens_per_s in the runtime snapshot is a counter total (NOT a rate) — see
		// metrics.go — so we must compute the delta here. Never use the raw total as a rate.
		genTotal := fieldVal(rt["generation_tokens_per_s"])
		genSupported := fieldSupported(rt["generation_tokens_per_s"])
		st.GenTokensPerSec, st.ServiceRateSupported = r.serviceRate(b.ID, genTotal, genSupported)
	}
	// /v1/cache — bounded cache-locality METADATA, materialized off the hot path.
	if c := r.getJSON(b.SnapshotURL + "/v1/cache"); c != nil {
		st.CacheObservationSupported, _ = c["supported"].(bool)
		st.CacheMatchSupported, _ = c["match_supported"].(bool)
		st.CacheReady, _ = c["ready"].(bool)
		st.CacheConfidence, _ = c["confidence"].(float64)
		if a, ok := c["snapshot_age_ms"].(float64); ok {
			st.CacheSnapshotAgeMs = int64(a)
		}
		if s, ok := c["last_event_sequence"].(float64); ok {
			st.CacheEventSequence = int64(s)
		}
		if e, ok := c["cache_reset_epoch"].(float64); ok {
			st.CacheResetEpoch = int64(e)
		}
		st.CacheIndexSize = intOf(c["index_entries"])
		st.CacheProvider, _ = c["provider"].(string)
		if hr, ok := c["kv_headroom"].(float64); ok {
			st.KVHeadroom = hr
		}
		st.KVHeadroomSupported, _ = c["kv_headroom_supported"].(bool)
		// directory (optional; only present + match-capable providers publish a non-empty one)
		if d, ok := c["directory"].(map[string]any); ok && len(d) > 0 {
			dir = make(map[string]int, len(d))
			for k, v := range d {
				dir[k] = intOf(v)
			}
		}
	}
	// Derive KV headroom from runtime KV util when the cache plane did not report it.
	if !st.KVHeadroomSupported && st.RuntimeHealthy {
		st.KVHeadroom = 1 - st.KVCacheUtil
		st.KVHeadroomSupported = true
	}
	return st, dir
}

// serviceRate computes generation tokens/sec by differencing a cumulative counter against the prior
// scrape. Returns (rate, supported). supported=false until two consecutive supported scrapes exist
// or when the counter is not exposed. Counter resets (current < prev) yield a 0 rate for that step.
func (r *Registry) serviceRate(backendID string, genTotal float64, supported bool) (float64, bool) {
	now := time.Now()
	r.prevMu.Lock()
	defer r.prevMu.Unlock()
	prev, had := r.prev[backendID]
	r.prev[backendID] = counterSample{genTokensTotal: genTotal, supported: supported, at: now}
	if !supported || !had || !prev.supported {
		return 0, false
	}
	dt := now.Sub(prev.at).Seconds()
	if dt <= 0 {
		return 0, false
	}
	d := genTotal - prev.genTokensTotal
	if d < 0 {
		return 0, true // counter reset (runtime restart): rate 0 this step, but supported.
	}
	return d / dt, true
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

// fieldSupported extracts .supported from a {value,supported} Field json object. A bare number is
// treated as supported (legacy shape).
func fieldSupported(v any) bool {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["supported"].(bool); ok {
			return s
		}
		return false
	}
	if _, ok := v.(float64); ok {
		return true
	}
	return false
}
