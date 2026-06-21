package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
	"github.com/lokic233/gpu-sidecar/internal/engine"
)

// apiMock is a minimal adapter for API-level tests.
type apiMock struct {
	mu      sync.Mutex
	visible bool
	access  bool
}

func (m *apiMock) Vendor() core.Vendor   { return core.VendorNVIDIA }
func (m *apiMock) Available() bool         { return true }
func (m *apiMock) DriverVersion() string   { return "mock" }
func (m *apiMock) RuntimeVersion() string  { return "mock" }
func (m *apiMock) Discover(t time.Duration) ([]core.Identity, error) {
	return []core.Identity{{DeviceID: "0", GPUModel: "Mock", GPUUUID: "u0", Vendor: core.VendorNVIDIA}}, nil
}
func (m *apiMock) Sample(d string, t time.Duration) (core.Health, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.visible {
		h := core.Health{GPUVisible: false}
		h.ProbeFailure = core.ProbeFailure{Class: core.FailureSoft, Reason: core.ReasonProbeFailure}
		return h, ""
	}
	return core.Health{GPUVisible: true, GPUAccessible: true,
		UtilizationGPU: core.Sup(10.0), MemUsedBytes: core.Sup(uint64(1e9)),
		MemFreeBytes: core.Sup(uint64(99e9)), MemTotalBytes: core.Sup(uint64(100e9)),
		TemperatureC: core.Sup(45.0), ComputeProcs: core.Sup(0), EffectiveFreeMemRatio: 0.99,
		Timestamp: time.Now()}, "raw"
}
func (m *apiMock) AccessProbe(d string, t time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.access
}

func setupServer(t *testing.T) (*Server, *engine.Supervisor, *apiMock) {
	mock := &apiMock{visible: true, access: true}
	sup := engine.NewSupervisor(mock, "inst", "host", "boot", "ver",
		core.DefaultLifecycleConfig(), core.DefaultStabilityConfig(), 120, 256, 256, 128, 2*time.Second, true)
	if err := sup.Init(nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	sup.PollOnce()
	sup.PollOnce()
	return New(sup, "test"), sup, mock
}

func do(srv *Server, method, target string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func TestAPI_Healthz(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "GET", "/healthz", "")
	if w.Code != 200 {
		t.Fatalf("healthz want 200 got %d", w.Code)
	}
}

func TestAPI_ReadyzHealthy(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "GET", "/readyz", "")
	if w.Code != 200 {
		t.Fatalf("readyz healthy want 200 got %d body=%s", w.Code, w.Body.String())
	}
	var res engine.ReadinessResult
	json.Unmarshal(w.Body.Bytes(), &res)
	if !res.Ready {
		t.Fatalf("expected ready, reasons=%v", res.Reasons)
	}
}

func TestAPI_ReadyzInaccessible(t *testing.T) {
	srv, sup, mock := setupServer(t)
	mock.mu.Lock(); mock.access = false; mock.mu.Unlock()
	sup.PollOnce()
	w := do(srv, "GET", "/readyz", "")
	if w.Code != 503 {
		t.Fatalf("readyz inaccessible want 503 got %d", w.Code)
	}
}

func TestAPI_Status(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "GET", "/v1/status", "")
	if w.Code != 200 {
		t.Fatalf("status want 200 got %d", w.Code)
	}
	var hs core.HostStatus
	if err := json.Unmarshal(w.Body.Bytes(), &hs); err != nil {
		t.Fatalf("status json: %v", err)
	}
	if len(hs.Devices) != 1 {
		t.Fatalf("want 1 device got %d", len(hs.Devices))
	}
	d := hs.Devices[0]
	if d.Capacity.CapacitySemantics != "heuristic_host_derived" {
		t.Fatalf("capacity must be labeled heuristic, got %q", d.Capacity.CapacitySemantics)
	}
	if d.Capacity.RuntimeServingCapacitySupported {
		t.Fatal("runtime serving capacity must be unsupported")
	}
	if len(d.Lifecycle.ReasonCodes) == 0 {
		t.Fatal("lifecycle reason codes must be present")
	}
}

func TestAPI_History(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "GET", "/v1/history", "")
	if w.Code != 200 {
		t.Fatalf("history want 200 got %d", w.Code)
	}
}

func TestAPI_Events(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "GET", "/v1/events", "")
	if w.Code != 200 {
		t.Fatalf("events want 200 got %d", w.Code)
	}
}

func TestAPI_DrainRejectsGET(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "GET", "/v1/drain?device=0&on=true", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("drain GET must be 405, got %d", w.Code)
	}
}

func TestAPI_DrainPostValid(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "POST", "/v1/drain", `{"device":"0","on":true}`)
	if w.Code != 200 {
		t.Fatalf("drain POST want 200 got %d body=%s", w.Code, w.Body.String())
	}
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if res["draining"] != true {
		t.Fatalf("expected draining=true, got %v", res)
	}
}

func TestAPI_DrainPostMissingFields(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "POST", "/v1/drain", `{"device":"0"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("drain missing 'on' want 400 got %d", w.Code)
	}
}

func TestAPI_DrainUnknownDevice(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "POST", "/v1/drain", `{"device":"99","on":true}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("drain unknown device want 404 got %d", w.Code)
	}
}

func TestAPI_DrainIdempotent(t *testing.T) {
	srv, _, _ := setupServer(t)
	do(srv, "POST", "/v1/drain", `{"device":"0","on":true}`)
	w := do(srv, "POST", "/v1/drain", `{"device":"0","on":true}`)
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if res["changed"] != false {
		t.Fatalf("repeated drain must be idempotent (changed=false), got %v", res)
	}
}

func TestAPI_Metrics(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "GET", "/metrics", "")
	if w.Code != 200 {
		t.Fatalf("metrics want 200 got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "gpu_host_capacity_hint") {
		t.Fatal("metrics must expose gpu_host_capacity_hint")
	}
	if strings.Contains(body, "gpu_effective_capacity") {
		t.Fatal("metrics must NOT expose old gpu_effective_capacity")
	}
}

func TestAPI_ConcurrentReadsDuringPolling(t *testing.T) {
	srv, sup, _ := setupServer(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// poller
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				sup.PollOnce()
			}
		}
	}()
	// concurrent readers
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				do(srv, "GET", "/v1/status", "")
				do(srv, "GET", "/readyz", "")
				do(srv, "GET", "/metrics", "")
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestAPI_UnsupportedFieldSerialization(t *testing.T) {
	srv, _, _ := setupServer(t)
	w := do(srv, "GET", "/v1/status", "")
	var hs core.HostStatus
	json.Unmarshal(w.Body.Bytes(), &hs)
	// power_limit unsupported on mock => must serialize with supported=false, not a fake value
	d := hs.Devices[0]
	if d.Health.PowerLimitWatts.Supported {
		t.Fatal("mock power limit should be unsupported")
	}
}
