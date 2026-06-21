package adapters

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
	"github.com/lokic233/gpu-sidecar/internal/exec"
)

// AMD adapter. Primary source: rocm-smi --json (amd-smi is permission-blocked on this host:
// "User is missing the following required groups: render, video"). RAS via --showrasinfo.
type AMD struct {
	rocmPath  string
	hasROCm   bool
	driver    string
	rocmVer   string
	model     string
	clkRe     *regexp.Regexp
}

func NewAMD() *AMD {
	a := &AMD{clkRe: regexp.MustCompile(`\((\d+)Mhz\)`)}
	if p, ok := exec.LookPath("rocm-smi"); ok {
		a.rocmPath, a.hasROCm = p, true
	}
	return a
}

func (a *AMD) Vendor() core.Vendor { return core.VendorAMD }
func (a *AMD) Available() bool      { return a.hasROCm }

func (a *AMD) DriverVersion() string {
	if a.driver != "" || !a.hasROCm {
		return a.driver
	}
	r := exec.Run(6*time.Second, a.rocmPath, "--showdriverversion", "--json")
	var m map[string]map[string]string
	if json.Unmarshal(r.Stdout, &m) == nil {
		if sys, ok := m["system"]; ok {
			a.driver = sys["Driver version"]
		}
	}
	return a.driver
}

func (a *AMD) RuntimeVersion() string {
	if a.rocmVer != "" {
		return a.rocmVer
	}
	// ROCm version best-effort from rocm-smi --version or /opt/rocm/.info/version
	r := exec.Run(5*time.Second, a.rocmPath, "--version")
	for _, line := range strings.Split(string(r.Stdout), "\n") {
		if strings.Contains(strings.ToLower(line), "rocm-smi version") || strings.Contains(line, "ROCM") {
			a.rocmVer = strings.TrimSpace(line)
			break
		}
	}
	if a.rocmVer == "" {
		a.rocmVer = "rocm(unknown)"
	}
	return a.rocmVer
}

func (a *AMD) cardKey(deviceID string) string { return "card" + deviceID }

func (a *AMD) Discover(timeout time.Duration) ([]core.Identity, error) {
	if !a.hasROCm {
		return nil, fmt.Errorf("rocm-smi unavailable")
	}
	r := exec.Run(timeout, a.rocmPath, "--showuniqueid", "--showserial", "--showproductname", "--json")
	if r.Err != nil {
		return nil, fmt.Errorf("rocm-smi discover: %v", r.Err)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal(r.Stdout, &m); err != nil {
		return nil, fmt.Errorf("rocm-smi json: %v", err)
	}
	drv := a.DriverVersion()
	var ids []core.Identity
	for k, v := range m {
		if !strings.HasPrefix(k, "card") {
			continue
		}
		devID := strings.TrimPrefix(k, "card")
		uuid := v["Unique ID"]
		if uuid == "" {
			uuid = v["Serial Number"]
		}
		model := firstNonEmpty(v["Card Series"], v["Card Model"], v["GFX Version"], "AMD Instinct")
		ids = append(ids, core.Identity{
			DeviceID:       devID,
			GPUUUID:        uuid,
			GPUModel:       model,
			DriverVersion:  drv,
			Vendor:         core.VendorAMD,
			RuntimeVersion: a.RuntimeVersion(),
		})
	}
	return ids, nil
}

func (a *AMD) Sample(deviceID string, timeout time.Duration) (core.Health, string) {
	h := core.Health{Timestamp: time.Now()}
	if !a.hasROCm {
		h.GPUVisible = false
		h.UnsupportedFields = append(h.UnsupportedFields, "all:rocm-smi-missing")
		h.ProbeFailure = core.ProbeFailure{Class: core.FailureHard, Reason: core.ReasonAdapterInitFailed, Detail: "rocm-smi not on PATH"}
		markUnsupportedAll(&h)
		return h, ""
	}
	start := time.Now()
	r := exec.Run(timeout, a.rocmPath, "-d", deviceID,
		"--showtemp", "--showuse", "--showpower", "--showmeminfo", "vram", "--showclocks", "--json")
	h.ProbeLatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
	raw := string(r.Stdout)
	if r.Err != nil || r.ExitCode != 0 {
		h.GPUVisible = false
		h.UnsupportedFields = append(h.UnsupportedFields, fmt.Sprintf("sample:exit=%d timedout=%v", r.ExitCode, r.TimedOut))
		h.ProbeFailure = classifyAMDFailure(r.TimedOut, string(r.Stderr))
		markUnsupportedAll(&h)
		return h, raw + "\nSTDERR:" + string(r.Stderr)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal(r.Stdout, &m); err != nil {
		h.GPUVisible = false
		h.UnsupportedFields = append(h.UnsupportedFields, "sample:json-parse-fail")
		// malformed JSON is a transient/parse SOFT failure, not proof the device is gone.
		h.ProbeFailure = core.ProbeFailure{Class: core.FailureSoft, Reason: core.ReasonProbeFailure, Detail: "rocm-smi json parse failed"}
		markUnsupportedAll(&h)
		return h, raw
	}
	card, ok := m[a.cardKey(deviceID)]
	if !ok {
		h.GPUVisible = false
		h.UnsupportedFields = append(h.UnsupportedFields, "sample:card-not-in-output")
		// card absent from rocm-smi output => device likely gone => HARD evidence.
		h.ProbeFailure = core.ProbeFailure{Class: core.FailureHard, Reason: core.ReasonDeviceDisappeared, Detail: "card not present in rocm-smi output"}
		markUnsupportedAll(&h)
		return h, raw
	}
	h.GPUVisible = true


	h.TemperatureC = amdF(card, "Temperature (Sensor junction) (C)")
	h.UtilizationGPU = amdF(card, "GPU use (%)")
	h.PowerWatts = amdF(card, "Current Socket Graphics Package Power (W)")
	h.SMClockMHz = a.clkMHz(card, "sclk clock speed:")
	h.MemClockMHz = a.clkMHz(card, "mclk clock speed:")

	total := amdU(card, "VRAM Total Memory (B)")
	used := amdU(card, "VRAM Total Used Memory (B)")
	h.MemTotalBytes = total
	h.MemUsedBytes = used
	if total.Supported && used.Supported {
		free := uint64(0)
		if total.Value > used.Value {
			free = total.Value - used.Value
		}
		h.MemFreeBytes = core.Sup(free)
		if total.Value > 0 {
			h.EffectiveFreeMemRatio = float64(free) / float64(total.Value)
		}
	} else {
		h.MemFreeBytes = core.Unsup[uint64]()
	}

	// Power limit not in this query set on rocm-smi here -> mark unsupported honestly.
	h.PowerLimitWatts = core.Unsup[float64]()
	h.UnsupportedFields = append(h.UnsupportedFields, "power_limit:rocm-smi-not-queried")

	// NVIDIA-only fields explicitly unsupported on AMD
	h.ECCUncorrectable = core.Unsup[uint64]()
	h.ECCCorrectable = core.Unsup[uint64]()
	h.XIDErrors = core.Unsup[[]int]()
	h.UnsupportedFields = append(h.UnsupportedFields, "nvidia_xid:amd-vendor", "nvidia_ecc:amd-uses-ras")

	// RAS (AMD error counters) via --showrasinfo
	unc, cor, rok := a.rasCounters(deviceID, timeout)
	if rok {
		h.RASUncorrectable = core.Sup(unc)
		h.RASCorrectable = core.Sup(cor)
	} else {
		h.RASUncorrectable = core.Unsup[uint64]()
		h.RASCorrectable = core.Unsup[uint64]()
		h.UnsupportedFields = append(h.UnsupportedFields, "amd_ras:unreadable")
	}

	// compute proc count for this device via --showpids
	h.ComputeProcs = a.computeProcs(deviceID, timeout)
	return h, raw
}

// rasCounters parses `rocm-smi --showrasinfo` table: sums Correctable/Uncorrectable across blocks.
func (a *AMD) rasCounters(deviceID string, timeout time.Duration) (unc, cor uint64, ok bool) {
	r := exec.Run(timeout, a.rocmPath, "-d", deviceID, "--showrasinfo")
	if r.Err != nil || r.ExitCode != 0 {
		return 0, 0, false
	}
	any := false
	for _, line := range strings.Split(string(r.Stdout), "\n") {
		fields := strings.Fields(line)
		// rows look like: <Block> ENABLED <correctable> <uncorrectable>
		if len(fields) >= 4 && (fields[1] == "ENABLED" || fields[1] == "DISABLED") {
			if c, e := strconv.ParseUint(fields[len(fields)-2], 10, 64); e == nil {
				cor += c
				any = true
			}
			if u, e := strconv.ParseUint(fields[len(fields)-1], 10, 64); e == nil {
				unc += u
				any = true
			}
		}
	}
	return unc, cor, any
}

func (a *AMD) computeProcs(deviceID string, timeout time.Duration) core.Field[int] {
	r := exec.Run(timeout, a.rocmPath, "--showpids", "--json")
	if r.Err != nil || r.ExitCode != 0 {
		return core.Unsup[int]()
	}
	var m map[string]map[string]string
	if json.Unmarshal(r.Stdout, &m) != nil {
		return core.Unsup[int]()
	}
	sys, ok := m["system"]
	if !ok {
		return core.Sup(0)
	}
	count := 0
	for k, v := range sys {
		if !strings.HasPrefix(k, "PID") {
			continue
		}
		// value: "name, gpu(s), vram, sdma, cu"  -> 2nd field is the gpu id(s)
		parts := strings.Split(v, ",")
		if len(parts) >= 2 {
			gpus := strings.TrimSpace(parts[1])
			if gpus == deviceID || strings.Contains(gpus, deviceID) {
				count++
			}
		}
	}
	return core.Sup(count)
}

func (a *AMD) clkMHz(card map[string]string, key string) core.Field[float64] {
	v, ok := card[key]
	if !ok {
		return core.Unsup[float64]()
	}
	m := a.clkRe.FindStringSubmatch(v)
	if len(m) < 2 {
		return core.Unsup[float64]()
	}
	if f, err := strconv.ParseFloat(m[1], 64); err == nil {
		return core.Sup(f)
	}
	return core.Unsup[float64]()
}

func (a *AMD) AccessProbe(deviceID string, timeout time.Duration) bool {
	r := exec.Run(timeout, a.rocmPath, "-d", deviceID, "--showid", "--json")
	if r.Err != nil || r.ExitCode != 0 {
		return false
	}
	var m map[string]map[string]string
	if json.Unmarshal(r.Stdout, &m) != nil {
		return false
	}
	_, ok := m[a.cardKey(deviceID)]
	return ok
}

// --- AMD helpers ---

func amdF(card map[string]string, key string) core.Field[float64] {
	v, ok := card[key]
	if !ok {
		return core.Unsup[float64]()
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
		return core.Sup(f)
	}
	return core.Unsup[float64]()
}

func amdU(card map[string]string, key string) core.Field[uint64] {
	v, ok := card[key]
	if !ok {
		return core.Unsup[uint64]()
	}
	if u, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
		return core.Sup(u)
	}
	// some keys are floats; try float then cast
	if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 {
		return core.Sup(uint64(f))
	}
	return core.Unsup[uint64]()
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// classifyAMDFailure decides hard vs soft from a failed rocm-smi sample.
func classifyAMDFailure(timedOut bool, stderr string) core.ProbeFailure {
	if timedOut {
		return core.ProbeFailure{Class: core.FailureSoft, Reason: core.ReasonProbeTimeout, Detail: "rocm-smi timed out"}
	}
	low := strings.ToLower(stderr)
	hardMarkers := []string{"no gpus", "device not found", "invalid device", "no such device", "not detected"}
	for _, m := range hardMarkers {
		if strings.Contains(low, m) {
			return core.ProbeFailure{Class: core.FailureHard, Reason: core.ReasonDeviceDisappeared, Detail: strings.TrimSpace(stderr)}
		}
	}
	return core.ProbeFailure{Class: core.FailureSoft, Reason: core.ReasonProbeFailure, Detail: strings.TrimSpace(stderr)}
}
