package config

import (
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

// Config is the sidecar runtime configuration.
type Config struct {
	ListenAddr      string        // default loopback-only (127.0.0.1:9095); override for trusted mesh
	PollInterval    time.Duration // base telemetry poll cadence
	ProbeTimeout    time.Duration // per vendor-command timeout
	AccessProbeEach bool          // run active access probe each cycle
	Devices         []string      // device ids to monitor; empty = all discovered
	WindowSeconds   float64       // reliability window
	ProbeRingCap    int
	PointRingCap    int
	EventRingCap    int
	WorkerCgroupHint string       // optional substring to identify worker processes
	EnableThroughputProbe bool    // run periodic micro-throughput probe (off by default; uses GPU)
	PersistPath     string        // optional append-only JSONL persistence ("" = disabled)

	Lifecycle core.LifecycleConfig
	Stability core.StabilityConfig
}

func Default() Config {
	return Config{
		// Loopback-only by default: the API includes an UNauthenticated mutation endpoint (/v1/drain).
		// Remote/mesh exposure requires an EXPLICIT --listen override on a trusted network. See
		// artifacts/api_security_notes.md.
		ListenAddr:      "127.0.0.1:9095",
		PollInterval:    2 * time.Second,
		ProbeTimeout:    8 * time.Second,
		AccessProbeEach: true,
		WindowSeconds:   120,
		ProbeRingCap:    2048,
		PointRingCap:    4096,
		EventRingCap:    512,
		EnableThroughputProbe: false,
		Lifecycle:       core.DefaultLifecycleConfig(),
		Stability:       core.DefaultStabilityConfig(),
	}
}

// IsLoopback reports whether a listen address binds only the loopback interface.
// Accepts forms like "127.0.0.1:9095", "[::1]:9095", "localhost:9095".
func IsLoopback(addr string) bool {
	host := addr
	// strip :port (handle [::1]:port and host:port)
	if i := lastColon(addr); i >= 0 {
		host = addr[:i]
	}
	host = trimBrackets(host)
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	// any 127.x.x.x is loopback
	if len(host) >= 4 && host[:4] == "127." {
		return true
	}
	return false
}

func lastColon(s string) int {
	idx := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			idx = i
		}
	}
	// for "[::1]:9095" the last colon is the port separator; for "[::1]" there is none after ]
	if r := lastIndexByte(s, ']'); r >= 0 && idx < r {
		return -1
	}
	return idx
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimBrackets(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}
