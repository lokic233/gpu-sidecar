package core

import "time"

// LifecycleConfig parameterizes the state machine. Documented in lifecycle_state_machine.md.
type LifecycleConfig struct {
	BusyUtilPct       float64 // util >= this => capacity-constrained candidate
	BusyMemRatio      float64 // free-mem-ratio <= this => capacity-constrained candidate
	DegradedScore     float64 // stability below this (with errors/latency) => DEGRADED
	OfflineFailures   int     // consecutive probe failures to declare OFFLINE
	ConfirmSamples    int     // hysteresis: N consecutive samples to confirm a non-failure transition
	RecoveringHoldSec float64 // min seconds in RECOVERING after coming back before READY/BUSY
}

func DefaultLifecycleConfig() LifecycleConfig {
	return LifecycleConfig{
		BusyUtilPct:       80.0,
		BusyMemRatio:      0.10,
		DegradedScore:     0.55,
		OfflineFailures:   3,
		ConfirmSamples:    2,
		RecoveringHoldSec: 5.0,
	}
}

// LifecycleMachine tracks one device's state with hysteresis and monotonic timing.
type LifecycleMachine struct {
	cfg          LifecycleConfig
	state        LifecycleState
	pending      LifecycleState
	pendingCount int
	draining     bool // operator-set drain flag
	offlineSince time.Duration
	recoverStart time.Duration
	hasBeenOffline bool
}

func NewLifecycleMachine(cfg LifecycleConfig) *LifecycleMachine {
	return &LifecycleMachine{cfg: cfg, state: StateUnknown, pending: StateUnknown}
}

func (m *LifecycleMachine) State() LifecycleState { return m.state }

// SetDraining marks the device for graceful drain (operator action).
func (m *LifecycleMachine) SetDraining(d bool) { m.draining = d }

// LifecycleObservation is the per-cycle input. mono is a monotonic clock reading.
type LifecycleObservation struct {
	ProbeOK             bool
	GPUVisible          bool
	GPUAccessible       bool
	ConsecutiveFailures int
	UtilPct             float64
	UtilSupported       bool
	FreeMemRatio        float64
	StabilityScore      float64
	NewUncorrErrors     uint64
	Mono                time.Duration // monotonic time since start
}

// desired computes the target state from a single observation (pre-hysteresis).
func (m *LifecycleMachine) desired(o LifecycleObservation) LifecycleState {
	// OFFLINE: enough consecutive failures or not visible/accessible.
	if !o.GPUVisible || !o.GPUAccessible || o.ConsecutiveFailures >= m.cfg.OfflineFailures {
		return StateOffline
	}
	// Operator drain takes precedence over normal states (but not OFFLINE).
	if m.draining {
		return StateDraining
	}
	// DEGRADED: low stability OR new uncorrectable errors, even if probe currently OK.
	if o.StabilityScore < m.cfg.DegradedScore || o.NewUncorrErrors > 0 {
		return StateDegraded
	}
	// BUSY: capacity-constrained (high util or low free mem).
	if (o.UtilSupported && o.UtilPct >= m.cfg.BusyUtilPct) || o.FreeMemRatio <= m.cfg.BusyMemRatio {
		return StateBusy
	}
	return StateReady
}

// Step advances the machine. Returns (newState, transitioned).
// Failure transitions (-> OFFLINE) are immediate; healthy transitions use hysteresis.
// After OFFLINE, the device passes through RECOVERING before READY/BUSY.
func (m *LifecycleMachine) Step(o LifecycleObservation) (LifecycleState, bool) {
	target := m.desired(o)
	prev := m.state

	// Immediate failure path: any OFFLINE target applies at once.
	if target == StateOffline {
		if m.state != StateOffline {
			m.offlineSince = o.Mono
			m.hasBeenOffline = true
		}
		m.state, m.pending, m.pendingCount = StateOffline, StateOffline, 0
		return m.state, prev != m.state
	}

	// Coming back from OFFLINE -> force RECOVERING first.
	if m.state == StateOffline {
		m.state = StateRecovering
		m.recoverStart = o.Mono
		m.pending, m.pendingCount = StateRecovering, 0
		return m.state, true
	}

	// In RECOVERING: hold for RecoveringHoldSec of healthy probes before promoting.
	if m.state == StateRecovering {
		held := (o.Mono - m.recoverStart).Seconds()
		if target == StateDegraded { // re-degraded during recovery
			m.state = StateDegraded
			return m.state, true
		}
		if held >= m.cfg.RecoveringHoldSec && o.ProbeOK {
			m.state = target // promote to READY/BUSY/DRAINING
			return m.state, prev != m.state
		}
		return m.state, false // stay RECOVERING
	}

	// Normal hysteresis for healthy-state changes.
	if target == m.state {
		m.pending, m.pendingCount = target, 0
		return m.state, false
	}
	if target == m.pending {
		m.pendingCount++
	} else {
		m.pending, m.pendingCount = target, 1
	}
	// DEGRADED applies fast (1 confirmation); others require ConfirmSamples.
	need := m.cfg.ConfirmSamples
	if target == StateDegraded || target == StateDraining {
		need = 1
	}
	if m.pendingCount >= need {
		m.state = target
		m.pendingCount = 0
		return m.state, true
	}
	return m.state, false
}

// HasRecovered reports whether the device has gone through an offline->recovering cycle.
func (m *LifecycleMachine) HasBeenOffline() bool { return m.hasBeenOffline }
