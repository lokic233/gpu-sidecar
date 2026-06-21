// Package adapters provides vendor-specific GPU telemetry behind one normalized interface.
package adapters

import (
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

// Adapter is the normalized vendor interface. Implementations MUST be defensive:
// never panic, always mark unsupported fields, never block beyond their timeouts.
type Adapter interface {
	// Vendor returns the vendor id.
	Vendor() core.Vendor
	// RuntimeVersion returns CUDA/ROCm version string (best effort).
	RuntimeVersion() string
	// DriverVersion returns the driver version (best effort).
	DriverVersion() string
	// Discover enumerates visible devices and their stable identity.
	Discover(timeout time.Duration) ([]core.Identity, error)
	// Sample collects an instantaneous health snapshot for the given device id.
	// It returns the Health plus the raw vendor output (for artifact preservation).
	Sample(deviceID string, timeout time.Duration) (core.Health, string)
	// AccessProbe actively confirms the device is reachable (lightweight).
	AccessProbe(deviceID string, timeout time.Duration) bool
}

// Available reports whether this adapter's primary vendor tool is present.
type Available interface {
	Available() bool
}
