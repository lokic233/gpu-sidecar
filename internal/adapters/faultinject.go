package adapters

import (
	"os"
	"sync/atomic"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

// FaultInject wraps a real adapter and can be told (via a control file) to simulate probe
// failures or delays for a specific device. This is for SAFE user-space churn experiments
// ONLY — it never touches hardware; it just makes Sample/AccessProbe report failure/slowness.
// Enabled only when GPU_SIDECAR_FAULT_FILE is set. Default: pass-through.
type FaultInject struct {
	inner    Adapter
	file     string
	failing  atomic.Bool
	delayMs  atomic.Int64
	device   string
}

// faultState is read from the control file each Sample: "fail:<dev>" or "delay:<dev>:<ms>" or "clear".
func WrapFaultInject(inner Adapter) Adapter {
	f := os.Getenv("GPU_SIDECAR_FAULT_FILE")
	if f == "" {
		return inner
	}
	return &FaultInject{inner: inner, file: f}
}

func (f *FaultInject) refresh(deviceID string) (mode string, delay time.Duration) {
	b, err := os.ReadFile(f.file)
	if err != nil {
		return "", 0
	}
	s := string(b)
	// parser: "fail <dev>" (soft), "failsoft <dev>", "failhard <dev>", "delay <dev> <ms>"
	for _, line := range splitLines(s) {
		ff := fields(line)
		if len(ff) >= 2 && ff[1] == deviceID {
			switch ff[0] {
			case "fail", "failsoft":
				return "soft", 0
			case "failhard":
				return "hard", 0
			case "delay":
				if len(ff) >= 3 {
					return "", time.Duration(atoiSafe(ff[2])) * time.Millisecond
				}
			}
		}
	}
	return "", 0
}

func (f *FaultInject) Vendor() core.Vendor        { return f.inner.Vendor() }
func (f *FaultInject) Available() bool {
	if a, ok := f.inner.(Available); ok {
		return a.Available()
	}
	return true
}
func (f *FaultInject) DriverVersion() string  { return f.inner.DriverVersion() }
func (f *FaultInject) RuntimeVersion() string { return f.inner.RuntimeVersion() }
func (f *FaultInject) Discover(t time.Duration) ([]core.Identity, error) {
	return f.inner.Discover(t)
}

func (f *FaultInject) Sample(deviceID string, t time.Duration) (core.Health, string) {
	mode, delay := f.refresh(deviceID)
	if delay > 0 {
		time.Sleep(delay)
	}
	if mode == "soft" {
		h := core.Health{Timestamp: time.Now(), GPUVisible: false}
		h.UnsupportedFields = append(h.UnsupportedFields, "fault-injected:soft-probe-failure")
		h.ProbeFailure = core.ProbeFailure{Class: core.FailureSoft, Reason: core.ReasonProbeFailure, Detail: "fault-injected soft failure (experiment)"}
		markUnsupportedAll(&h)
		return h, "FAULT_INJECTED soft probe failure (experiment)"
	}
	if mode == "hard" {
		h := core.Health{Timestamp: time.Now(), GPUVisible: false}
		h.UnsupportedFields = append(h.UnsupportedFields, "fault-injected:hard-device-gone")
		h.ProbeFailure = core.ProbeFailure{Class: core.FailureHard, Reason: core.ReasonDeviceDisappeared, Detail: "fault-injected hard failure (experiment)"}
		markUnsupportedAll(&h)
		return h, "FAULT_INJECTED hard device-gone (experiment)"
	}
	return f.inner.Sample(deviceID, t)
}

func (f *FaultInject) AccessProbe(deviceID string, t time.Duration) bool {
	mode, _ := f.refresh(deviceID)
	if mode == "soft" || mode == "hard" {
		return false
	}
	return f.inner.AccessProbe(deviceID, t)
}

// tiny string helpers (avoid extra imports)
func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
func fields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}
