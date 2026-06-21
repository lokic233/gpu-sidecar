package adapters

import (
	"fmt"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

// Generic is the fallback adapter when no GPU vendor tool is present. It reports a single
// pseudo-device that is visible-but-unsupported, so the sidecar still serves a truthful
// "sidecar alive, GPU inaccessible" contract instead of crashing.
type Generic struct{}

func NewGeneric() *Generic           { return &Generic{} }
func (g *Generic) Vendor() core.Vendor { return core.VendorUnknown }
func (g *Generic) Available() bool      { return true }
func (g *Generic) DriverVersion() string { return "" }
func (g *Generic) RuntimeVersion() string { return "" }

func (g *Generic) Discover(timeout time.Duration) ([]core.Identity, error) {
	return []core.Identity{{DeviceID: "0", GPUModel: "none", Vendor: core.VendorUnknown, GPUUUID: "generic-0"}}, nil
}

func (g *Generic) Sample(deviceID string, timeout time.Duration) (core.Health, string) {
	h := core.Health{Timestamp: time.Now(), GPUVisible: false}
	h.UnsupportedFields = append(h.UnsupportedFields, "all:no-vendor-tool")
	markUnsupportedAll(&h)
	return h, fmt.Sprintf("generic adapter: no vendor GPU tooling on device %s", deviceID)
}

func (g *Generic) AccessProbe(deviceID string, timeout time.Duration) bool { return false }
