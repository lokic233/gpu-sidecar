package adapters

import (
	"strings"
	"testing"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

func TestNVIDIACSVHelpers(t *testing.T) {
	f := splitCSV("3, GPU-abc, NVIDIA H100, 580.82.07, 4, 97000, 97871, 0, 32, 67.5, 500, 345, 1593, 0, 0")
	if len(f) != 15 { t.Fatalf("want 15 fields got %d", len(f)) }
	if f[0] != "3" || f[2] != "NVIDIA H100" { t.Fatalf("trim/parse wrong: %v", f[:3]) }
}

func TestParseFHandlesNA(t *testing.T) {
	cases := map[string]bool{"67.5": true, "[N/A]": false, "[Not Supported]": false, "": false, "abc": false, "0": true}
	for in, wantOK := range cases {
		_, ok := parseF(in)
		if ok != wantOK { t.Fatalf("parseF(%q) ok=%v want %v", in, ok, wantOK) }
	}
}

func TestMibToBytesUnsupported(t *testing.T) {
	f := mibToBytes(parseF("[N/A]"))
	if f.Supported { t.Fatal("N/A must be unsupported") }
	f2 := mibToBytes(parseF("100"))
	if !f2.Supported || f2.Value != 100*1024*1024 { t.Fatalf("100MiB wrong: %+v", f2) }
}

func TestNVIDIAMissingTool(t *testing.T) {
	n := &NVIDIA{hasSMI: false}
	h, _ := n.Sample("0", time.Second)
	if h.GPUVisible { t.Fatal("missing nvidia-smi must yield GPUVisible=false") }
	if h.UtilizationGPU.Supported { t.Fatal("fields must be unsupported when tool missing") }
	if len(h.UnsupportedFields) == 0 { t.Fatal("must record unsupported reason") }
}

func TestAMDClockParse(t *testing.T) {
	a := NewAMD()
	card := map[string]string{"sclk clock speed:": "(1393Mhz)"}
	f := a.clkMHz(card, "sclk clock speed:")
	if !f.Supported || f.Value != 1393 { t.Fatalf("clk parse wrong: %+v", f) }
	bad := a.clkMHz(map[string]string{"x": "garbage"}, "sclk clock speed:")
	if bad.Supported { t.Fatal("missing clk must be unsupported") }
}

func TestAMDFieldHelpers(t *testing.T) {
	card := map[string]string{"GPU use (%)": "42", "VRAM Total Memory (B)": "309220868096", "bad": "xx"}
	if f := amdF(card, "GPU use (%)"); !f.Supported || f.Value != 42 { t.Fatalf("amdF wrong %+v", f) }
	if f := amdF(card, "missing"); f.Supported { t.Fatal("missing must be unsupported") }
	if f := amdU(card, "VRAM Total Memory (B)"); !f.Supported || f.Value != 309220868096 { t.Fatalf("amdU wrong %+v", f) }
	if f := amdF(card, "bad"); f.Supported { t.Fatal("garbage must be unsupported") }
}

func TestMarkUnsupportedAllNoCrash(t *testing.T) {
	var h core.Health
	markUnsupportedAll(&h)
	if h.UtilizationGPU.Supported || h.MemUsedBytes.Supported || h.XIDErrors.Supported {
		t.Fatal("markUnsupportedAll must clear all supported flags")
	}
}

func TestRASParse(t *testing.T) {
	out := `RAS INFO
         Block       Status    Correctable Error  Uncorrectable Error
           UMC        ENABLED                  2                    1
           GFX        ENABLED                  0                    0`
	// reuse parsing logic inline (mirror of rasCounters table parse)
	var unc, cor uint64
	for _, line := range strings.Split(out, "\n") {
		ff := strings.Fields(line)
		if len(ff) >= 4 && (ff[1] == "ENABLED" || ff[1] == "DISABLED") {
			cor += mustU(ff[len(ff)-2])
			unc += mustU(ff[len(ff)-1])
		}
	}
	if cor != 2 || unc != 1 { t.Fatalf("RAS parse wrong cor=%d unc=%d", cor, unc) }
}

func mustU(s string) uint64 { var v uint64; for _, c := range s { if c < '0' || c > '9' { return 0 }; v = v*10 + uint64(c-'0') }; return v }
