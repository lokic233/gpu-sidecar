package config

import (
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

// Config is the sidecar runtime configuration.
type Config struct {
	ListenAddr      string        // e.g. "[::]:9095"
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
		ListenAddr:      "[::]:9095",
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
