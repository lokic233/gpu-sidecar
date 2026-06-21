package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
	"github.com/lokic233/gpu-sidecar/internal/engine"
)

// multiMock supports per-device visible/access control.
type multiMock struct {
	mu      sync.Mutex
	devices []string
	visible map[string]bool
	access  map[string]bool
}

func newMultiMock(devs ...string) *multiMock {
	m := &multiMock{devices: devs, visible: map[string]bool{}, access: map[string]bool{}}
	for _, d := range devs {
		m.visible[d] = true
		m.access[d] = true
	}
	return m
}
func (m *multiMock) Vendor() core.Vendor  { return core.VendorNVIDIA }
func (m *multiMock) Available() bool        { return true }
func (m *multiMock) DriverVersion() string  { return "mock" }
func (m *multiMock) RuntimeVersion() string { return "mock" }
func (m *multiMock) Discover(t time.Duration) ([]core.Identity, error) {
	var ids []core.Identity
	for _, d := range m.devices {
		ids = append(ids, core.Identity{DeviceID: d, GPUModel: "Mock", GPUUUID: "u" + d, Vendor: core.VendorNVIDIA})
	}
	return ids, nil
}
func (m *multiMock) Sample(d string, t time.Duration) (core.Health, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.visible[d] {
		h := core.Health{GPUVisible: false}
		h.ProbeFailure = core.ProbeFailure{Class: core.FailureSoft, Reason: core.ReasonProbeFailure}
		return h, ""
	}
	return core.Health{GPUVisible: true, GPUAccessible: m.access[d],
		UtilizationGPU: core.Sup(10.0), MemUsedBytes: core.Sup(uint64(1e9)),
		MemFreeBytes: core.Sup(uint64(99e9)), MemTotalBytes: core.Sup(uint64(100e9)),
		ComputeProcs: core.Sup(0), EffectiveFreeMemRatio: 0.99, Timestamp: time.Now()}, "raw"
}
func (m *multiMock) AccessProbe(d string, t time.Duration) bool {
	m.mu.Lock(); defer m.mu.Unlock(); return m.access[d]
}

func setupMulti(t *testing.T, devs ...string) (*Server, *engine.Supervisor, *multiMock) {
	mock := newMultiMock(devs...)
	sup := engine.NewSupervisor(mock, "inst", "host", "boot", "ver",
		core.DefaultLifecycleConfig(), core.DefaultStabilityConfig(), 120, 256, 256, 128, 2*time.Second, true)
	if err := sup.Init(nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	sup.PollOnce(); sup.PollOnce()
	return New(sup, "test"), sup, mock
}

func TestReadyz_AllDevicesReady(t *testing.T) {
	srv, _, _ := setupMulti(t, "0", "1", "2")
	w := do(srv, "GET", "/readyz", "")
	if w.Code != 200 {
		t.Fatalf("all ready want 200 got %d", w.Code)
	}
	var res engine.ReadinessResult
	json.Unmarshal(w.Body.Bytes(), &res)
	if !res.ControlPlaneReady || !res.AnyDeviceReady || !res.AllDevicesReady {
		t.Fatalf("all ready expected all flags true: %+v", res)
	}
	if res.ReadyDeviceCount != 3 || res.TotalDeviceCount != 3 {
		t.Fatalf("want 3/3 ready, got %d/%d", res.ReadyDeviceCount, res.TotalDeviceCount)
	}
}

func TestReadyz_SomeDevicesReady(t *testing.T) {
	srv, sup, mock := setupMulti(t, "0", "1", "2")
	mock.mu.Lock(); mock.access["1"] = false; mock.visible["1"] = false; mock.mu.Unlock()
	sup.PollOnce()
	w := do(srv, "GET", "/readyz", "")
	if w.Code != 200 {
		t.Fatalf("partial ready: host should still be 200 (control-plane), got %d", w.Code)
	}
	var res engine.ReadinessResult
	json.Unmarshal(w.Body.Bytes(), &res)
	if !res.ControlPlaneReady {
		t.Fatal("control plane should be ready (2/3 ok)")
	}
	if !res.AnyDeviceReady {
		t.Fatal("any_device_ready should be true")
	}
	if res.AllDevicesReady {
		t.Fatal("all_devices_ready must be FALSE with one bad device")
	}
	if res.ReadyDeviceCount != 2 {
		t.Fatalf("want 2 ready, got %d", res.ReadyDeviceCount)
	}
}

func TestReadyz_NoDevicesReady(t *testing.T) {
	srv, sup, mock := setupMulti(t, "0", "1")
	mock.mu.Lock()
	for _, d := range []string{"0", "1"} { mock.visible[d] = false; mock.access[d] = false }
	mock.mu.Unlock()
	// drive to OFFLINE
	for i := 0; i < 4; i++ { sup.PollOnce() }
	w := do(srv, "GET", "/readyz", "")
	if w.Code != 503 {
		t.Fatalf("no devices ready want 503 got %d", w.Code)
	}
	var res engine.ReadinessResult
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.AnyDeviceReady || res.AllDevicesReady || res.ControlPlaneReady {
		t.Fatalf("nothing should be ready: %+v", res)
	}
}

func TestReadyz_PerDevice_Ready(t *testing.T) {
	srv, _, _ := setupMulti(t, "0", "1")
	w := do(srv, "GET", "/readyz?device=0", "")
	if w.Code != 200 {
		t.Fatalf("per-device ready want 200 got %d", w.Code)
	}
	var dr engine.DeviceReadiness
	json.Unmarshal(w.Body.Bytes(), &dr)
	if !dr.Ready || dr.DeviceID != "0" {
		t.Fatalf("device 0 should be ready: %+v", dr)
	}
}

func TestReadyz_PerDevice_NotReady(t *testing.T) {
	srv, sup, mock := setupMulti(t, "0", "1")
	mock.mu.Lock(); mock.access["1"] = false; mock.visible["1"] = false; mock.mu.Unlock()
	sup.PollOnce()
	w := do(srv, "GET", "/readyz?device=1", "")
	if w.Code != 503 {
		t.Fatalf("per-device not-ready want 503 got %d", w.Code)
	}
	var dr engine.DeviceReadiness
	json.Unmarshal(w.Body.Bytes(), &dr)
	if dr.Ready {
		t.Fatal("device 1 should not be ready")
	}
	if len(dr.Reasons) == 0 {
		t.Fatal("device 1 must carry reasons")
	}
}

func TestReadyz_PerDevice_Invalid(t *testing.T) {
	srv, _, _ := setupMulti(t, "0", "1")
	w := do(srv, "GET", "/readyz?device=99", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("invalid device want 404 got %d", w.Code)
	}
}

func TestReadyz_OneInaccessibleDevice(t *testing.T) {
	srv, sup, mock := setupMulti(t, "0", "1", "2")
	// device 2 visible but access probe fails => soft failure
	mock.mu.Lock(); mock.access["2"] = false; mock.mu.Unlock()
	sup.PollOnce()
	var res engine.ReadinessResult
	json.Unmarshal(do(srv, "GET", "/readyz", "").Body.Bytes(), &res)
	if res.AllDevicesReady {
		t.Fatal("an inaccessible device must make all_devices_ready false")
	}
	// device 2 specifically not ready
	w := do(srv, "GET", "/readyz?device=2", "")
	if w.Code != 503 {
		t.Fatalf("inaccessible device want 503 got %d", w.Code)
	}
}
