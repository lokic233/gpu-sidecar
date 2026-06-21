package core

import "time"

// Reason codes explain WHY a lifecycle decision was made (exposed in LifecycleInfo.ReasonCodes).
const (
	ReasonHealthy            = "HEALTHY"
	ReasonGPUNotVisible      = "GPU_NOT_VISIBLE"            // hard-offline evidence
	ReasonGPUAccessProbeFail = "GPU_ACCESS_PROBE_FAILED"    // soft-failure evidence
	ReasonProbeTimeout       = "PROBE_TIMEOUT"              // soft-failure evidence
	ReasonProbeFailure       = "PROBE_FAILURE"              // soft-failure evidence
	ReasonOfflineThreshold   = "OFFLINE_FAILURE_THRESHOLD_REACHED"
	ReasonAdapterInitFailed  = "ADAPTER_INIT_FAILED"        // hard-offline evidence
	ReasonDeviceDisappeared  = "DEVICE_DISAPPEARED"         // hard-offline evidence
	ReasonLowStability       = "LOW_STABILITY_SCORE"
	ReasonUncorrErrors       = "UNCORRECTABLE_ERRORS"
	ReasonHighUtil           = "HIGH_UTILIZATION"
	ReasonLowFreeMem         = "LOW_FREE_MEMORY"
	ReasonOperatorDrain      = "OPERATOR_DRAIN"
	ReasonRecoveryHold       = "RECOVERY_HOLD"
	ReasonRecoveryStreakMet  = "RECOVERY_STREAK_MET"
)

// LifecycleConfig parameterizes the state machine. Documented in lifecycle_hysteresis.md.
type LifecycleConfig struct {
	BusyUtilPct       float64 // util >= this => capacity-constrained candidate
	BusyMemRatio      float64 // free-mem-ratio <= this => capacity-constrained candidate
	DegradedScore     float64 // stability below this => DEGRADED
	OfflineFailures   int     // consecutive SOFT failures required to declare OFFLINE
	ConfirmSamples    int     // hysteresis: N consecutive samples to confirm a healthy state change
	RecoveringHoldSec float64 // min seconds in RECOVERING before READY/BUSY
	RecoveryStreak    int     // min consecutive healthy probes in RECOVERING before promotion
}

func DefaultLifecycleConfig() LifecycleConfig {
	return LifecycleConfig{
		BusyUtilPct:       80.0,
		BusyMemRatio:      0.10,
		DegradedScore:     0.55,
		OfflineFailures:   3,
		ConfirmSamples:    2,
		RecoveringHoldSec: 5.0,
		RecoveryStreak:    3,
	}
}

// LifecycleMachine tracks one device's state with hysteresis and monotonic timing.
type LifecycleMachine struct {
	cfg          LifecycleConfig
	state        LifecycleState
	pending      LifecycleState
	pendingCount int
	draining     bool

	softFailures int  // consecutive transient soft failures (probe/access/timeout/malformed)
	healthyStreak int // consecutive healthy probes (used for recovery promotion)
	hardOffline  bool // whether current OFFLINE was entered via definitive hard evidence

	offlineSince   time.Duration
	recoverStart   time.Duration
	hasBeenOffline bool

	reasonCodes []string // reasons for the most recent decision
}

func NewLifecycleMachine(cfg LifecycleConfig) *LifecycleMachine {
	return &LifecycleMachine{cfg: cfg, state: StateUnknown, pending: StateUnknown}
}

func (m *LifecycleMachine) State() LifecycleState { return m.state }
func (m *LifecycleMachine) SetDraining(d bool)    { m.draining = d }
func (m *LifecycleMachine) Draining() bool        { return m.draining }
func (m *LifecycleMachine) HasBeenOffline() bool  { return m.hasBeenOffline }

// Info returns an auditable snapshot of the machine's reasoning.
func (m *LifecycleMachine) Info() LifecycleInfo {
	rc := append([]string(nil), m.reasonCodes...)
	if len(rc) == 0 {
		rc = []string{ReasonHealthy}
	}
	return LifecycleInfo{
		State:                   m.state,
		ReasonCodes:             rc,
		ConsecutiveSoftFailures: m.softFailures,
		OfflineFailureThreshold: m.cfg.OfflineFailures,
		HealthyStreak:           m.healthyStreak,
		RecoveryStreakRequired:  m.cfg.RecoveryStreak,
		HardOffline:             m.hardOffline,
	}
}

// LifecycleObservation is the per-cycle input. Mono is a monotonic clock reading.
type LifecycleObservation struct {
	ProbeOK       bool
	GPUVisible    bool
	GPUAccessible bool

	// Distinguish HARD vs SOFT failure evidence.
	// HardOfflineEvidence: device disappeared from enumeration, adapter init failed, runtime
	// definitively reports no device. These transition to OFFLINE immediately.
	HardOfflineEvidence bool
	// SoftFailure: a single transient failure (CLI timeout, one failed access probe, malformed
	// output). These go DEGRADED first and require OfflineFailures consecutive failures for OFFLINE.
	SoftFailure bool

	UtilPct         float64
	UtilSupported   bool
	FreeMemRatio    float64
	StabilityScore  float64
	NewUncorrErrors uint64
	Mono            time.Duration

	// ProbeFailReason is a reason code for the current soft/hard failure (for auditing).
	ProbeFailReason string
}

// classifyHealthy decides the target healthy/busy/degraded/draining state (no failures present).
func (m *LifecycleMachine) classifyHealthy(o LifecycleObservation) (LifecycleState, []string) {
	if m.draining {
		return StateDraining, []string{ReasonOperatorDrain}
	}
	if o.StabilityScore < m.cfg.DegradedScore {
		return StateDegraded, []string{ReasonLowStability}
	}
	if o.NewUncorrErrors > 0 {
		return StateDegraded, []string{ReasonUncorrErrors}
	}
	if o.UtilSupported && o.UtilPct >= m.cfg.BusyUtilPct {
		return StateBusy, []string{ReasonHighUtil}
	}
	if o.FreeMemRatio <= m.cfg.BusyMemRatio {
		return StateBusy, []string{ReasonLowFreeMem}
	}
	return StateReady, []string{ReasonHealthy}
}

// Step advances the machine. Returns (newState, transitioned).
//
// Semantics:
//   - HARD offline evidence (device gone / adapter init fail) => OFFLINE immediately.
//   - SOFT failures => DEGRADED first; only after OfflineFailures consecutive soft failures => OFFLINE.
//   - Recovery from OFFLINE => RECOVERING, held until RecoveringHoldSec AND RecoveryStreak healthy probes.
func (m *LifecycleMachine) Step(o LifecycleObservation) (LifecycleState, bool) {
	prev := m.state

	// Determine failure condition. The adapter's explicit classification takes precedence:
	// if SoftFailure is set, treat as soft even if GPUVisible is false (e.g. a transient sample
	// failure that returned no data). Only treat !GPUVisible as HARD evidence when the failure was
	// NOT explicitly classified soft.
	hard := o.HardOfflineEvidence || (!o.GPUVisible && !o.SoftFailure)
	soft := !hard && (o.SoftFailure || !o.GPUAccessible || !o.ProbeOK)

	if hard {
		m.softFailures = 0
		m.healthyStreak = 0
		m.reasonCodes = m.hardReasons(o)
		if m.state != StateOffline {
			m.offlineSince = o.Mono
			m.hasBeenOffline = true
		}
		m.hardOffline = true
		m.state, m.pending, m.pendingCount = StateOffline, StateOffline, 0
		return m.state, prev != m.state
	}

	if soft {
		m.softFailures++
		m.healthyStreak = 0
		// Threshold reached => OFFLINE (soft-offline).
		if m.softFailures >= m.cfg.OfflineFailures {
			m.reasonCodes = []string{ReasonOfflineThreshold, m.softReason(o)}
			if m.state != StateOffline {
				m.offlineSince = o.Mono
				m.hasBeenOffline = true
			}
			m.hardOffline = false
			m.state, m.pending, m.pendingCount = StateOffline, StateOffline, 0
			return m.state, prev != m.state
		}
		// Below threshold => DEGRADED (unless already OFFLINE, stay OFFLINE).
		if m.state == StateOffline {
			m.reasonCodes = []string{m.softReason(o)}
			return m.state, false
		}
		m.reasonCodes = []string{m.softReason(o)}
		changed := m.state != StateDegraded
		m.state, m.pending, m.pendingCount = StateDegraded, StateDegraded, 0
		return m.state, changed
	}

	// --- Healthy probe path ---
	m.softFailures = 0
	m.healthyStreak++

	// Recovery from OFFLINE: pass through RECOVERING.
	if m.state == StateOffline {
		m.state = StateRecovering
		m.recoverStart = o.Mono
		m.pending, m.pendingCount = StateRecovering, 0
		m.reasonCodes = []string{ReasonRecoveryHold}
		return m.state, true
	}

	if m.state == StateRecovering {
		held := (o.Mono - m.recoverStart).Seconds()
		target, reasons := m.classifyHealthy(o)
		if target == StateDegraded { // re-degraded during recovery
			m.state = StateDegraded
			m.reasonCodes = reasons
			return m.state, true
		}
		// Promote only after BOTH hold time AND healthy streak satisfied.
		if held >= m.cfg.RecoveringHoldSec && m.healthyStreak >= m.cfg.RecoveryStreak {
			m.state = target
			m.reasonCodes = append([]string{ReasonRecoveryStreakMet}, reasons...)
			return m.state, prev != m.state
		}
		m.reasonCodes = []string{ReasonRecoveryHold}
		return m.state, false // stay RECOVERING
	}

	// Normal hysteresis for healthy-state changes.
	target, reasons := m.classifyHealthy(o)
	if target == m.state {
		m.pending, m.pendingCount = target, 0
		m.reasonCodes = reasons
		return m.state, false
	}
	// Leaving DEGRADED to a healthy target applies immediately: the degrading condition
	// (soft failure / low stability / errors) has cleared on this probe. This is the documented
	// "one successful probe -> READY" recovery from a transient soft failure. It is NOT OFFLINE
	// flapping (OFFLINE recovery is gated through RECOVERING separately).
	if m.state == StateDegraded && (target == StateReady || target == StateBusy) {
		m.state = target
		m.pending, m.pendingCount = target, 0
		m.reasonCodes = reasons
		return m.state, true
	}
	if target == m.pending {
		m.pendingCount++
	} else {
		m.pending, m.pendingCount = target, 1
	}
	need := m.cfg.ConfirmSamples
	if target == StateDegraded || target == StateDraining {
		need = 1 // safety-relevant: apply fast
	}
	if m.pendingCount >= need {
		m.state = target
		m.pendingCount = 0
		m.reasonCodes = reasons
		return m.state, true
	}
	// pending but not confirmed; keep current state + reason
	m.reasonCodes = reasons
	return m.state, false
}

func (m *LifecycleMachine) hardReasons(o LifecycleObservation) []string {
	var r []string
	if !o.GPUVisible {
		r = append(r, ReasonGPUNotVisible)
	}
	if o.HardOfflineEvidence {
		if o.ProbeFailReason != "" {
			r = append(r, o.ProbeFailReason)
		} else {
			r = append(r, ReasonDeviceDisappeared)
		}
	}
	if len(r) == 0 {
		r = []string{ReasonDeviceDisappeared}
	}
	return r
}

func (m *LifecycleMachine) softReason(o LifecycleObservation) string {
	if o.ProbeFailReason != "" {
		return o.ProbeFailReason
	}
	if !o.GPUAccessible {
		return ReasonGPUAccessProbeFail
	}
	return ReasonProbeFailure
}
