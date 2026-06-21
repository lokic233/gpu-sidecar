// Package core defines the normalized cross-vendor data contract for the GPU host sidecar.
package core

import "time"

// Vendor identifies the GPU backend vendor.
type Vendor string

const (
	VendorNVIDIA  Vendor = "nvidia"
	VendorAMD     Vendor = "amd"
	VendorUnknown Vendor = "unknown"
)

// LifecycleState is the explicit per-device state-machine state.
type LifecycleState string

const (
	StateUnknown    LifecycleState = "UNKNOWN"
	StateReady      LifecycleState = "READY"
	StateBusy       LifecycleState = "BUSY"
	StateDegraded   LifecycleState = "DEGRADED"
	StateDraining   LifecycleState = "DRAINING"
	StateOffline    LifecycleState = "OFFLINE"
	StateRecovering LifecycleState = "RECOVERING"
)

// Field carries a value that may be unsupported on a given vendor/tool.
// Supported=false means the metric could not be obtained; consumers MUST check it
// rather than trusting the zero value. This is how we avoid fabricating metrics.
type Field[T any] struct {
	Value     T    `json:"value"`
	Supported bool `json:"supported"`
}

func Sup[T any](v T) Field[T]   { return Field[T]{Value: v, Supported: true} }
func Unsup[T any]() Field[T]    { var z T; return Field[T]{Value: z, Supported: false} }

// Identity is the stable identity of a device/backend. Rarely changes.
type Identity struct {
	SidecarInstanceID string `json:"sidecar_instance_id"`
	Hostname          string `json:"hostname"`
	BackendID         string `json:"backend_id"`
	Vendor            Vendor `json:"vendor"`
	DeviceID          string `json:"device_id"`  // vendor index, e.g. "3"
	GPUModel          string `json:"gpu_model"`
	GPUUUID           string `json:"gpu_uuid"`   // stable vendor id (UUID on NVIDIA, unique_id/serial on AMD)
	DriverVersion     string `json:"driver_version"`
	RuntimeVersion    string `json:"runtime_version"` // CUDA / ROCm
	BootID            string `json:"boot_id"`
	SidecarVersion    string `json:"sidecar_version"`
}

// ProbeFailure classifies a failed telemetry sample. Class is "" on success.
type ProbeFailure struct {
	Class  string `json:"class"`  // "" | "hard" | "soft"
	Reason string `json:"reason"` // a reason code (see lifecycle reason codes)
	Detail string `json:"detail"`
}

// Probe failure classes.
const (
	FailureNone = ""
	FailureHard = "hard" // device gone / adapter init failed / definitively no device
	FailureSoft = "soft" // transient: timeout, one failed access probe, malformed output
)

// Health is the instantaneous health + capacity snapshot for one device.
type Health struct {
	Timestamp        time.Time `json:"timestamp"`
	HeartbeatOK      bool      `json:"heartbeat_ok"`
	GPUVisible       bool      `json:"gpu_visible"`
	GPUAccessible    bool      `json:"gpu_accessible"` // result of access probe

	UtilizationGPU   Field[float64] `json:"utilization_gpu_pct"`
	MemUsedBytes     Field[uint64]  `json:"mem_used_bytes"`
	MemFreeBytes     Field[uint64]  `json:"mem_free_bytes"`
	MemTotalBytes    Field[uint64]  `json:"mem_total_bytes"`
	TemperatureC     Field[float64] `json:"temperature_c"`
	PowerWatts       Field[float64] `json:"power_watts"`
	PowerLimitWatts  Field[float64] `json:"power_limit_watts"`
	SMClockMHz       Field[float64] `json:"sm_clock_mhz"`
	MemClockMHz      Field[float64] `json:"mem_clock_mhz"`
	ComputeProcs     Field[int]     `json:"compute_proc_count"`

	EffectiveFreeMemRatio float64 `json:"effective_free_mem_ratio"` // [0,1], derived
	ProbeLatencyMs        float64 `json:"probe_latency_ms"`         // telemetry collection latency

	// ProbeFailure classifies WHY a sample failed (empty when the sample succeeded). It lets the
	// lifecycle machine distinguish hard-offline evidence from transient soft failures.
	ProbeFailure ProbeFailure `json:"probe_failure"`

	// Vendor error counters
	ECCUncorrectable Field[uint64] `json:"ecc_uncorrectable_total"`
	ECCCorrectable   Field[uint64] `json:"ecc_correctable_total"`
	XIDErrors        Field[[]int]  `json:"nvidia_xid_errors"`        // NVIDIA only
	RASUncorrectable Field[uint64] `json:"amd_ras_uncorrectable"`    // AMD only
	RASCorrectable   Field[uint64] `json:"amd_ras_correctable"`      // AMD only

	// Notes lists fields that were unsupported/unavailable this cycle (human-readable).
	UnsupportedFields []string `json:"unsupported_fields"`
}

// Reliability is the historical reliability accounting for one device.
type Reliability struct {
	LastSuccessfulProbe  *time.Time `json:"last_successful_probe"`
	LastFailedProbe      *time.Time `json:"last_failed_probe"`
	ConsecutiveFailures  int        `json:"consecutive_probe_failures"`
	SidecarStartCount    int        `json:"sidecar_start_count"`
	WorkerStarts         int        `json:"worker_starts_observed"`
	WorkerStops          int        `json:"worker_stops_observed"`
	DisconnectCount      int        `json:"disconnect_count"`
	RejoinCount          int        `json:"rejoin_count"`
	LastRecoveryMs       float64    `json:"last_recovery_duration_ms"`
	RecentAvailability   float64    `json:"recent_availability_ratio"`   // [0,1] over window
	RecentFailureRate    float64    `json:"recent_failure_rate"`         // [0,1] over window
	ProbeLatencyP50Ms    float64    `json:"probe_latency_p50_ms"`
	ProbeLatencyP95Ms    float64    `json:"probe_latency_p95_ms"`
	ThroughputVariance   Field[float64] `json:"throughput_variance"`     // from controlled probe, when enabled
}

// StabilityScore is the normalized [0,1] operational signal plus its audit components.
type StabilityScore struct {
	Score      float64            `json:"score"`       // [0,1]
	Components map[string]float64 `json:"components"`  // each contribution, for auditing
	UpdatedAt  time.Time          `json:"updated_at"`
}

// DeviceStatus is the full normalized status of one device.
type DeviceStatus struct {
	Identity       Identity       `json:"identity"`
	Health         Health         `json:"health"`
	LifecycleState LifecycleState `json:"lifecycle_state"`
	Lifecycle      LifecycleInfo  `json:"lifecycle"` // reason codes + hysteresis counters (audit)
	Reliability    Reliability    `json:"reliability"`
	Stability      StabilityScore `json:"stability"`

	// Capacity is a HOST-DERIVED HEURISTIC, NOT runtime serving capacity. See capacity_semantics.md.
	Capacity CapacityHint `json:"capacity"`
}

// LifecycleInfo exposes WHY a device is in its current lifecycle state, for auditing.
type LifecycleInfo struct {
	State                  LifecycleState `json:"state"`
	ReasonCodes            []string       `json:"reason_codes"`
	ConsecutiveSoftFailures int           `json:"consecutive_soft_failures"`
	OfflineFailureThreshold int           `json:"offline_failure_threshold"`
	HealthyStreak          int            `json:"healthy_streak"`
	RecoveryStreakRequired int            `json:"recovery_streak_required"`
	HardOffline            bool           `json:"hard_offline"` // true if entered via definitive hard-offline evidence
	RecoveryLatched        bool           `json:"recovery_latched"`        // true while a post-OFFLINE device must pass hold+streak
	RecoveryHealthyStreak  int            `json:"recovery_healthy_streak"` // consecutive healthy probes accrued toward release
}

// CapacityHint is an explicitly-heuristic, host-derived pressure signal.
// It is NOT a measurement of how many requests/tokens a backend can serve. A future runtime
// plugin (see RuntimeServingCapacity) is required for true serving capacity.
type CapacityHint struct {
	HostCapacityHint           float64            `json:"host_capacity_hint"`         // [0,1] heuristic headroom
	CapacitySemantics          string             `json:"capacity_semantics"`         // always "heuristic_host_derived"
	Components                 map[string]float64 `json:"components"`                 // free_memory_ratio, utilization_headroom, stability_score
	RuntimeServingCapacitySupported bool          `json:"runtime_serving_capacity_supported"` // false: no runtime plugin connected
	RuntimeServingCapacity     *RuntimeServingCapacity `json:"runtime_serving_capacity,omitempty"`
}

// RuntimeServingCapacity is the OPTIONAL runtime-plugin-supplied view. Nil unless a runtime
// adapter (vLLM/TGI/etc.) is connected. A pure host sidecar cannot observe these reliably.
type RuntimeServingCapacity struct {
	Source            string  `json:"source"`             // e.g. "vllm-metrics"
	QueueDepth        Field[int]     `json:"queue_depth"`
	KVCacheUtilization Field[float64] `json:"kv_cache_utilization"`
	MaxAdmissibleBatch Field[int]    `json:"max_admissible_batch"`
	ActiveSequences   Field[int]     `json:"active_sequences"`
	TTFTms            Field[float64] `json:"ttft_ms"`
	TPOTms            Field[float64] `json:"tpot_ms"`
}

// HostStatus is the top-level sidecar response: one host, N devices.
type HostStatus struct {
	SidecarInstanceID string         `json:"sidecar_instance_id"`
	Hostname          string         `json:"hostname"`
	Vendor            Vendor         `json:"vendor"`
	SidecarVersion    string         `json:"sidecar_version"`
	BootID            string         `json:"boot_id"`
	Timestamp         time.Time      `json:"timestamp"`
	UptimeSeconds     float64        `json:"uptime_seconds"`
	Devices           []DeviceStatus `json:"devices"`
}

// Event is a bounded transition/failure event for /v1/events.
// Event is a bounded transition/failure event. Evidence (what was observed) is kept SEPARATE
// from inference (what it might mean). See worker_event_semantics.md.
type Event struct {
	Timestamp time.Time      `json:"timestamp"`
	DeviceID  string         `json:"device_id"`
	Kind      string         `json:"kind"` // see Event kind constants below
	From      LifecycleState `json:"from,omitempty"`
	To        LifecycleState `json:"to,omitempty"`
	Detail    string         `json:"detail"`

	// Evidence holds raw observed facts (counts, bytes). Inference fields below are derived.
	Evidence map[string]any `json:"evidence,omitempty"`
	// TerminationCause is only set for worker-disappearance events. "unknown" unless the sidecar
	// has DIRECT evidence (supervised exit status, cgroup/runtime event, kernel error).
	TerminationCause string `json:"termination_cause,omitempty"`
	// GroundTruthSource names an authoritative source if one confirmed the cause (else null/empty).
	GroundTruthSource string  `json:"ground_truth_source,omitempty"`
	ReasonCodes       []string `json:"reason_codes,omitempty"`
}

// Event kind constants. Worker events separate OBSERVATION from CONFIRMED CAUSE.
const (
	EventStateTransition = "STATE_TRANSITION"
	EventProbeFailure    = "PROBE_FAILURE"
	EventProbeRecovered  = "PROBE_RECOVERED"
	EventDisconnect      = "DISCONNECT"
	EventRejoin          = "REJOIN"
	EventErrorCounter    = "ERROR_COUNTER"

	// Worker lifecycle — evidence-only by default:
	EventWorkerObserved   = "WORKER_OBSERVED"    // a compute process is present
	EventWorkerStarted    = "WORKER_STARTED"     // proc count / memory increased
	EventWorkerDisappeared = "WORKER_DISAPPEARED" // proc count / memory decreased — cause UNKNOWN
	// The following require DIRECT evidence and are NOT emitted by the pure host sidecar today:
	EventWorkerExitObserved      = "WORKER_EXIT_OBSERVED"       // supervised exit status seen
	EventWorkerCrashConfirmed    = "WORKER_CRASH_CONFIRMED"     // confirmed abnormal exit
	EventWorkerOOMConfirmed      = "WORKER_OOM_CONFIRMED"       // confirmed OOM
	EventWorkerTerminationUnknown = "WORKER_TERMINATION_CAUSE_UNKNOWN"
)

// TerminationCause values.
const (
	CauseUnknown   = "unknown"
	CauseGraceful  = "graceful_exit_observed" // only when supervised; not inferable from count delta
	CauseCrash     = "crash_confirmed"
	CauseOOM       = "oom_confirmed"
)


// HistoryPoint is a bounded time-series sample for /v1/history.
type HistoryPoint struct {
	Timestamp      time.Time      `json:"timestamp"`
	DeviceID       string         `json:"device_id"`
	LifecycleState LifecycleState `json:"lifecycle_state"`
	StabilityScore float64        `json:"stability_score"`
	UtilGPU        float64        `json:"utilization_gpu_pct"`
	MemUsedBytes   uint64         `json:"mem_used_bytes"`
	TemperatureC   float64        `json:"temperature_c"`
	ProbeOK        bool           `json:"probe_ok"`
	ProbeLatencyMs float64        `json:"probe_latency_ms"`
}
