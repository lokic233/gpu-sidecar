package adapters

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
	"github.com/lokic233/gpu-sidecar/internal/exec"
)

// NVIDIA adapter: primary source nvidia-smi --query-gpu (CSV), plus dmesg XID scrape.
type NVIDIA struct {
	smiPath   string
	hasSMI    bool
	driver    string
	cuda      string
	xidRe     *regexp.Regexp
}

func NewNVIDIA() *NVIDIA {
	n := &NVIDIA{xidRe: regexp.MustCompile(`NVRM: Xid \(PCI:[^)]*\): (\d+)`)}
	if p, ok := exec.LookPath("nvidia-smi"); ok {
		n.smiPath, n.hasSMI = p, true
	}
	return n
}

func (n *NVIDIA) Vendor() core.Vendor { return core.VendorNVIDIA }
func (n *NVIDIA) Available() bool      { return n.hasSMI }

func (n *NVIDIA) DriverVersion() string {
	if n.driver != "" {
		return n.driver
	}
	if !n.hasSMI {
		return ""
	}
	r := exec.Run(5*time.Second, n.smiPath, "--query-gpu=driver_version", "--format=csv,noheader,nounits")
	if r.ExitCode == 0 {
		n.driver = firstLine(string(r.Stdout))
	}
	return n.driver
}

func (n *NVIDIA) RuntimeVersion() string {
	if n.cuda != "" {
		return n.cuda
	}
	r := exec.Run(5*time.Second, n.smiPath, "--query")
	// CUDA version is in `nvidia-smi -q` header; fall back to parsing nvidia-smi banner.
	r2 := exec.Run(5*time.Second, n.smiPath)
	for _, line := range strings.Split(string(r2.Stdout), "\n") {
		if i := strings.Index(line, "CUDA Version:"); i >= 0 {
			n.cuda = strings.TrimSpace(strings.TrimRight(line[i+len("CUDA Version:"):], "| "))
			break
		}
	}
	_ = r
	return n.cuda
}

const nvQuery = "index,uuid,name,driver_version,memory.used,memory.free,memory.total," +
	"utilization.gpu,temperature.gpu,power.draw,power.limit,clocks.sm,clocks.mem," +
	"ecc.errors.uncorrected.aggregate.total,ecc.errors.corrected.aggregate.total"

func (n *NVIDIA) Discover(timeout time.Duration) ([]core.Identity, error) {
	if !n.hasSMI {
		return nil, fmt.Errorf("nvidia-smi unavailable")
	}
	r := exec.Run(timeout, n.smiPath, "--query-gpu="+nvQuery, "--format=csv,noheader,nounits")
	if r.Err != nil {
		return nil, fmt.Errorf("nvidia-smi discover: %v", r.Err)
	}
	var ids []core.Identity
	sc := bufio.NewScanner(strings.NewReader(string(r.Stdout)))
	for sc.Scan() {
		f := splitCSV(sc.Text())
		if len(f) < 4 {
			continue
		}
		ids = append(ids, core.Identity{
			DeviceID:       f[0],
			GPUUUID:        f[1],
			GPUModel:       f[2],
			DriverVersion:  f[3],
			Vendor:         core.VendorNVIDIA,
			RuntimeVersion: n.RuntimeVersion(),
		})
	}
	return ids, nil
}

func (n *NVIDIA) Sample(deviceID string, timeout time.Duration) (core.Health, string) {
	h := core.Health{Timestamp: time.Now()}
	if !n.hasSMI {
		h.GPUVisible = false
		h.UnsupportedFields = append(h.UnsupportedFields, "all:nvidia-smi-missing")
		markUnsupportedAll(&h)
		return h, ""
	}
	start := time.Now()
	r := exec.Run(timeout, n.smiPath, "-i", deviceID, "--query-gpu="+nvQuery, "--format=csv,noheader,nounits")
	h.ProbeLatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
	raw := string(r.Stdout)
	if r.Err != nil || r.ExitCode != 0 {
		h.GPUVisible = false
		h.UnsupportedFields = append(h.UnsupportedFields, fmt.Sprintf("sample:exit=%d timedout=%v", r.ExitCode, r.TimedOut))
		markUnsupportedAll(&h)
		return h, raw + "\nSTDERR:" + string(r.Stderr)
	}
	f := splitCSV(firstLine(raw))
	if len(f) < 15 {
		h.GPUVisible = false
		h.UnsupportedFields = append(h.UnsupportedFields, "sample:short-output")
		markUnsupportedAll(&h)
		return h, raw
	}
	h.GPUVisible = true
	// 0 idx,1 uuid,2 name,3 drv,4 mem.used,5 mem.free,6 mem.total,7 util,8 temp,
	// 9 power.draw,10 power.limit,11 sm_clk,12 mem_clk,13 ecc.uncorr,14 ecc.corr
	h.MemUsedBytes = mibToBytes(parseF(f[4]))
	h.MemFreeBytes = mibToBytes(parseF(f[5]))
	h.MemTotalBytes = mibToBytes(parseF(f[6]))
	h.UtilizationGPU = supF(f[7])
	h.TemperatureC = supF(f[8])
	h.PowerWatts = supF(f[9])
	h.PowerLimitWatts = supF(f[10])
	h.SMClockMHz = supF(f[11])
	h.MemClockMHz = supF(f[12])
	h.ECCUncorrectable = supU(f[13], &h, "ecc_uncorrectable")
	h.ECCCorrectable = supU(f[14], &h, "ecc_correctable")
	// AMD-only fields explicitly unsupported on NVIDIA
	h.RASUncorrectable = core.Unsup[uint64]()
	h.RASCorrectable = core.Unsup[uint64]()
	h.UnsupportedFields = append(h.UnsupportedFields, "amd_ras:nvidia-vendor")

	// compute process count for this device
	h.ComputeProcs = n.computeProcs(deviceID, timeout)
	// effective free mem ratio
	if h.MemTotalBytes.Supported && h.MemTotalBytes.Value > 0 {
		h.EffectiveFreeMemRatio = float64(h.MemFreeBytes.Value) / float64(h.MemTotalBytes.Value)
	}
	// XID errors (best effort, dmesg)
	h.XIDErrors = n.xidErrors()
	return h, raw
}

func (n *NVIDIA) computeProcs(deviceID string, timeout time.Duration) core.Field[int] {
	r := exec.Run(timeout, n.smiPath, "-i", deviceID, "--query-compute-apps=pid", "--format=csv,noheader")
	if r.Err != nil || r.ExitCode != 0 {
		return core.Unsup[int]()
	}
	count := 0
	for _, line := range strings.Split(string(r.Stdout), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return core.Sup(count)
}

// xidErrors scrapes dmesg for recent NVRM Xid lines (best-effort, may be empty/unreadable).
func (n *NVIDIA) xidErrors() core.Field[[]int] {
	r := exec.Run(4*time.Second, "dmesg")
	if r.Err != nil || r.ExitCode != 0 {
		return core.Unsup[[]int]()
	}
	var xids []int
	for _, m := range n.xidRe.FindAllStringSubmatch(string(r.Stdout), -1) {
		if v, err := strconv.Atoi(m[1]); err == nil {
			xids = append(xids, v)
		}
	}
	return core.Sup(xids)
}

func (n *NVIDIA) AccessProbe(deviceID string, timeout time.Duration) bool {
	r := exec.Run(timeout, n.smiPath, "-i", deviceID, "--query-gpu=uuid", "--format=csv,noheader")
	return r.ExitCode == 0 && r.Err == nil && strings.TrimSpace(string(r.Stdout)) != ""
}

// --- helpers ---

func markUnsupportedAll(h *core.Health) {
	h.UtilizationGPU = core.Unsup[float64]()
	h.MemUsedBytes = core.Unsup[uint64]()
	h.MemFreeBytes = core.Unsup[uint64]()
	h.MemTotalBytes = core.Unsup[uint64]()
	h.TemperatureC = core.Unsup[float64]()
	h.PowerWatts = core.Unsup[float64]()
	h.PowerLimitWatts = core.Unsup[float64]()
	h.SMClockMHz = core.Unsup[float64]()
	h.MemClockMHz = core.Unsup[float64]()
	h.ComputeProcs = core.Unsup[int]()
	h.ECCUncorrectable = core.Unsup[uint64]()
	h.ECCCorrectable = core.Unsup[uint64]()
	h.XIDErrors = core.Unsup[[]int]()
	h.RASUncorrectable = core.Unsup[uint64]()
	h.RASCorrectable = core.Unsup[uint64]()
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// parseF parses a float, treating "[N/A]", "[Not Supported]" etc. as NaN-ish (-1 sentinel handled by caller).
func parseF(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") || strings.EqualFold(s, "N/A") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func supF(s string) core.Field[float64] {
	if v, ok := parseF(s); ok {
		return core.Sup(v)
	}
	return core.Unsup[float64]()
}

func supU(s string, h *core.Health, name string) core.Field[uint64] {
	if v, ok := parseF(s); ok && v >= 0 {
		return core.Sup(uint64(v))
	}
	h.UnsupportedFields = append(h.UnsupportedFields, name+":not-reported")
	return core.Unsup[uint64]()
}

func mibToBytes(v float64, ok bool) core.Field[uint64] {
	if !ok {
		return core.Unsup[uint64]()
	}
	return core.Sup(uint64(v * 1024 * 1024))
}

var _ = os.Getenv // reserved for future env-based overrides
