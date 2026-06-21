package engine

import (
	"sync"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

// mockAdapter is a programmable adapter for deterministic supervisor tests.
type mockAdapter struct {
	mu        sync.Mutex
	vendor    core.Vendor
	ids       []core.Identity
	health    map[string]core.Health // deviceID -> health to return
	access    map[string]bool        // deviceID -> access probe result
	discErr   error
}

func newMockAdapter(devices ...string) *mockAdapter {
	m := &mockAdapter{vendor: core.VendorNVIDIA, health: map[string]core.Health{}, access: map[string]bool{}}
	for _, d := range devices {
		m.ids = append(m.ids, core.Identity{DeviceID: d, GPUModel: "MockGPU", GPUUUID: "uuid-" + d, Vendor: core.VendorNVIDIA})
		m.setHealthy(d)
	}
	return m
}

func (m *mockAdapter) Vendor() core.Vendor      { return m.vendor }
func (m *mockAdapter) Available() bool           { return true }
func (m *mockAdapter) DriverVersion() string     { return "mock-1.0" }
func (m *mockAdapter) RuntimeVersion() string    { return "mock-rt" }
func (m *mockAdapter) Discover(t time.Duration) ([]core.Identity, error) {
	return m.ids, m.discErr
}
func (m *mockAdapter) Sample(deviceID string, t time.Duration) (core.Health, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.health[deviceID]
	h.Timestamp = time.Now()
	return h, "mock-raw"
}
func (m *mockAdapter) AccessProbe(deviceID string, t time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.access[deviceID]; ok {
		return v
	}
	return true
}

func (m *mockAdapter) setHealthy(d string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.health[d] = core.Health{
		GPUVisible: true, GPUAccessible: true,
		UtilizationGPU: core.Sup(5.0), MemUsedBytes: core.Sup(uint64(1e9)),
		MemFreeBytes: core.Sup(uint64(90e9)), MemTotalBytes: core.Sup(uint64(100e9)),
		TemperatureC: core.Sup(40.0), ComputeProcs: core.Sup(0),
		ECCUncorrectable: core.Sup(uint64(0)), EffectiveFreeMemRatio: 0.9,
	}
	m.access[d] = true
}

func (m *mockAdapter) setSoftFail(d string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := core.Health{GPUVisible: false}
	h.ProbeFailure = core.ProbeFailure{Class: core.FailureSoft, Reason: core.ReasonProbeFailure, Detail: "mock soft"}
	m.health[d] = h
	m.access[d] = false
}

func (m *mockAdapter) setHardFail(d string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := core.Health{GPUVisible: false}
	h.ProbeFailure = core.ProbeFailure{Class: core.FailureHard, Reason: core.ReasonDeviceDisappeared, Detail: "mock hard"}
	m.health[d] = h
	m.access[d] = false
}

func (m *mockAdapter) setProcs(d string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.health[d]
	h.ComputeProcs = core.Sup(n)
	m.health[d] = h
}

func (m *mockAdapter) setMemUsed(d string, used uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.health[d]
	h.MemUsedBytes = core.Sup(used)
	if h.MemTotalBytes.Supported && h.MemTotalBytes.Value > 0 {
		h.MemFreeBytes = core.Sup(h.MemTotalBytes.Value - used)
		h.EffectiveFreeMemRatio = float64(h.MemTotalBytes.Value-used) / float64(h.MemTotalBytes.Value)
	}
	m.health[d] = h
}

func (m *mockAdapter) setUtil(d string, pct float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.health[d]
	h.UtilizationGPU = core.Sup(pct)
	m.health[d] = h
}
