package vllm

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	rt "github.com/lokic233/gpu-sidecar/internal/runtime"
)

// Config configures the vLLM runtime adapter.
type Config struct {
	BaseURL              string
	HealthPath           string
	MetricsPath          string
	ChatCompletionsPath  string
	RequestTimeout       time.Duration
	ConnectTimeout       time.Duration
	MetricsTimeout       time.Duration
	MetricsRefresh       time.Duration // runtime metrics loop cadence (faster than HW telemetry)
	HealthRefresh        time.Duration
}

func DefaultConfig() Config {
	return Config{
		BaseURL:             "http://127.0.0.1:8000",
		HealthPath:          "/health",
		MetricsPath:         "/metrics",
		ChatCompletionsPath: "/v1/chat/completions",
		RequestTimeout:      10 * time.Minute,
		ConnectTimeout:      2 * time.Second,
		MetricsTimeout:      500 * time.Millisecond,
		MetricsRefresh:      1 * time.Second,
		HealthRefresh:       2 * time.Second,
	}
}

// Adapter implements runtime.RuntimeAdapter for vLLM. The proxy/data-plane HTTP client is SEPARATE
// from the metrics/health client so a slow metrics scrape never blocks request streaming.
type Adapter struct {
	cfg Config

	metricsClient *http.Client
	healthClient  *http.Client
	// ProxyClient is exported for the data plane (long timeouts, connection pooling, streaming).
	ProxyClient *http.Client

	snap   atomic.Pointer[rt.RuntimeMetrics]
	healthy atomic.Bool

	stop chan struct{}
	wg   sync.WaitGroup

	modelName atomic.Pointer[string]
	version   string
}

func New(cfg Config) *Adapter {
	a := &Adapter{
		cfg: cfg,
		metricsClient: &http.Client{Timeout: cfg.MetricsTimeout},
		healthClient:  &http.Client{Timeout: cfg.ConnectTimeout + 500*time.Millisecond},
		ProxyClient: &http.Client{
			Timeout: cfg.RequestTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
				// Disable response buffering-friendly settings for streaming:
				DisableCompression:  true,
				ForceAttemptHTTP2:   false,
			},
		},
		stop: make(chan struct{}),
	}
	empty := rt.RuntimeMetrics{Timestamp: time.Now(), RawMetricSources: map[string]string{}}
	a.snap.Store(&empty)
	return a
}

func (a *Adapter) Type() string { return "vllm" }
func (a *Adapter) Health() bool { return a.healthy.Load() }
func (a *Adapter) BaseURL() string { return a.cfg.BaseURL }
func (a *Adapter) ChatCompletionsURL() string { return a.cfg.BaseURL + a.cfg.ChatCompletionsPath }
func (a *Adapter) Config() Config { return a.cfg }

func (a *Adapter) Snapshot() rt.RuntimeMetrics {
	p := a.snap.Load()
	if p == nil {
		return rt.RuntimeMetrics{}
	}
	m := *p
	m.ScrapeAgeMs = float64(time.Since(m.Timestamp).Microseconds()) / 1000.0
	m.Healthy = a.healthy.Load()
	return m
}

func (a *Adapter) Start() {
	a.wg.Add(2)
	go a.metricsLoop()
	go a.healthLoop()
}

func (a *Adapter) Stop() {
	close(a.stop)
	a.wg.Wait()
}

func (a *Adapter) healthLoop() {
	defer a.wg.Done()
	t := time.NewTicker(a.cfg.HealthRefresh)
	defer t.Stop()
	a.checkHealth()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			a.checkHealth()
		}
	}
}

func (a *Adapter) checkHealth() {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.ConnectTimeout+500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", a.cfg.BaseURL+a.cfg.HealthPath, nil)
	resp, err := a.healthClient.Do(req)
	if err != nil {
		a.healthy.Store(false)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	a.healthy.Store(resp.StatusCode == 200)
}

func (a *Adapter) metricsLoop() {
	defer a.wg.Done()
	t := time.NewTicker(a.cfg.MetricsRefresh)
	defer t.Stop()
	a.scrape()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			a.scrape()
		}
	}
}

func (a *Adapter) scrape() {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.MetricsTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", a.cfg.BaseURL+a.cfg.MetricsPath, nil)
	resp, err := a.metricsClient.Do(req)
	m := rt.RuntimeMetrics{Timestamp: time.Now(), RawMetricSources: map[string]string{}, RuntimeVersion: a.version}
	if mn := a.modelName.Load(); mn != nil {
		m.ModelName = *mn
	}
	if err != nil {
		// keep last good snapshot's identity but mark scrape failed
		m.ScrapeOK = false
		m.ScrapeLatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
		m.UnsupportedFields = append(m.UnsupportedFields, "scrape:error:"+err.Error())
		a.snap.Store(&m)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	m.ScrapeLatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
	if resp.StatusCode != 200 {
		m.ScrapeOK = false
		m.UnsupportedFields = append(m.UnsupportedFields, "scrape:status:"+resp.Status)
		a.snap.Store(&m)
		return
	}
	m.ScrapeOK = true
	mapVLLMMetrics(string(body), &m)
	a.snap.Store(&m)
}

// SetModelName/SetVersion let the launcher record provenance discovered at startup.
func (a *Adapter) SetModelName(n string) { a.modelName.Store(&n) }
func (a *Adapter) SetVersion(v string)   { a.version = v }
