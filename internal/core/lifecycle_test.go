package core

import (
	"testing"
	"time"
)

// helper: build a healthy observation
func healthyObs(mono time.Duration) LifecycleObservation {
	return LifecycleObservation{ProbeOK: true, GPUVisible: true, GPUAccessible: true,
		UtilPct: 5, UtilSupported: true, FreeMemRatio: 0.9, StabilityScore: 0.95, Mono: mono}
}

// helper: soft-failure observation (transient)
func softFailObs(mono time.Duration) LifecycleObservation {
	return LifecycleObservation{ProbeOK: false, GPUVisible: true, GPUAccessible: false,
		SoftFailure: true, ProbeFailReason: ReasonGPUAccessProbeFail, StabilityScore: 0.95, Mono: mono}
}

// helper: hard-offline observation (device gone)
func hardFailObs(mono time.Duration) LifecycleObservation {
	return LifecycleObservation{ProbeOK: false, GPUVisible: false, GPUAccessible: false,
		HardOfflineEvidence: true, ProbeFailReason: ReasonDeviceDisappeared, Mono: mono}
}

func settle(m *LifecycleMachine, mono *time.Duration, n int) {
	for i := 0; i < n; i++ {
		*mono += time.Second
		m.Step(healthyObs(*mono))
	}
}

// REQUIRED SEQUENCE 1: READY -> one soft failure -> DEGRADED -> successful probe -> READY
func TestLifecycle_SoftFailureGoesDegradedNotOffline(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	if m.State() != StateReady {
		t.Fatalf("want READY after settle, got %s", m.State())
	}
	// one soft failure
	mono += time.Second
	st, _ := m.Step(softFailObs(mono))
	if st != StateDegraded {
		t.Fatalf("one soft failure must go DEGRADED (not OFFLINE), got %s", st)
	}
	if m.Info().ConsecutiveSoftFailures != 1 {
		t.Fatalf("want 1 soft failure, got %d", m.Info().ConsecutiveSoftFailures)
	}
	// successful probe -> back to READY
	mono += time.Second
	st, _ = m.Step(healthyObs(mono))
	if st != StateReady {
		t.Fatalf("want READY after recovery probe, got %s", st)
	}
}

// REQUIRED SEQUENCE 2: READY -> repeated soft failures below threshold -> DEGRADED -> threshold -> OFFLINE
func TestLifecycle_SoftFailuresReachThresholdOffline(t *testing.T) {
	cfg := DefaultLifecycleConfig() // OfflineFailures=3
	m := NewLifecycleMachine(cfg)
	var mono time.Duration
	settle(m, &mono, 3)
	// failure 1 -> DEGRADED
	mono += time.Second
	if st, _ := m.Step(softFailObs(mono)); st != StateDegraded {
		t.Fatalf("failure 1 want DEGRADED got %s", st)
	}
	// failure 2 -> still DEGRADED (below threshold)
	mono += time.Second
	if st, _ := m.Step(softFailObs(mono)); st != StateDegraded {
		t.Fatalf("failure 2 want DEGRADED got %s", st)
	}
	if m.Info().ConsecutiveSoftFailures != 2 {
		t.Fatalf("want 2 soft failures, got %d", m.Info().ConsecutiveSoftFailures)
	}
	// failure 3 -> OFFLINE (threshold reached)
	mono += time.Second
	if st, _ := m.Step(softFailObs(mono)); st != StateOffline {
		t.Fatalf("failure 3 (threshold) want OFFLINE got %s", st)
	}
	rc := m.Info().ReasonCodes
	found := false
	for _, c := range rc {
		if c == ReasonOfflineThreshold {
			found = true
		}
	}
	if !found {
		t.Fatalf("OFFLINE reason must include threshold code, got %v", rc)
	}
	if m.Info().HardOffline {
		t.Fatal("soft-threshold OFFLINE must NOT be flagged hard_offline")
	}
}

// REQUIRED SEQUENCE 3: READY -> definitive device disappearance -> OFFLINE immediately
func TestLifecycle_HardEvidenceImmediateOffline(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	mono += time.Second
	st, transitioned := m.Step(hardFailObs(mono))
	if st != StateOffline {
		t.Fatalf("hard evidence must go OFFLINE immediately, got %s", st)
	}
	if !transitioned {
		t.Fatal("should report transition")
	}
	if !m.Info().HardOffline {
		t.Fatal("hard OFFLINE must be flagged hard_offline=true")
	}
}

// REQUIRED SEQUENCE 4: OFFLINE -> one successful probe -> RECOVERING -> sustained healthy -> READY
func TestLifecycle_RecoveryThroughRecovering(t *testing.T) {
	cfg := DefaultLifecycleConfig() // RecoveringHoldSec=5, RecoveryStreak=3
	m := NewLifecycleMachine(cfg)
	var mono time.Duration
	settle(m, &mono, 3)
	// drive to OFFLINE via hard evidence
	mono += time.Second
	m.Step(hardFailObs(mono))
	if m.State() != StateOffline {
		t.Fatalf("setup: want OFFLINE got %s", m.State())
	}
	// one successful probe -> RECOVERING (NOT READY)
	mono += time.Second
	st, _ := m.Step(healthyObs(mono))
	if st != StateRecovering {
		t.Fatalf("one good probe after OFFLINE must go RECOVERING, got %s", st)
	}
	// must NOT promote before both hold time AND streak satisfied
	mono += time.Second
	if st, _ = m.Step(healthyObs(mono)); st != StateRecovering {
		t.Fatalf("must stay RECOVERING (streak/hold not met), got %s", st)
	}
	// advance enough healthy probes + time
	for i := 0; i < 6; i++ {
		mono += 2 * time.Second
		st, _ = m.Step(healthyObs(mono))
	}
	if st != StateReady {
		t.Fatalf("want READY after sustained recovery, got %s", st)
	}
}

// A single good probe must NOT flip OFFLINE->READY directly (no flapping).
func TestLifecycle_NoOfflineReadyFlapping(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	mono += time.Second
	m.Step(hardFailObs(mono))
	mono += time.Second
	st, _ := m.Step(healthyObs(mono))
	if st == StateReady {
		t.Fatal("OFFLINE must not jump straight to READY on one probe")
	}
}

// Single-sample util spike must not flip READY->BUSY (hysteresis).
func TestLifecycle_NoBusyFlapping(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	transitions := 0
	step := func(o LifecycleObservation) { mono += time.Second; o.Mono = mono; if _, tr := m.Step(o); tr { transitions++ } }
	base := transitions
	busy := healthyObs(0); busy.UtilPct = 95
	step(busy)               // spike
	step(healthyObs(0))      // back to idle
	step(busy)               // spike
	step(healthyObs(0))      // back to idle
	if transitions > base {
		t.Fatalf("single-sample spikes caused %d transitions (flapping)", transitions-base)
	}
}

// Sustained high util -> BUSY after confirmation.
func TestLifecycle_SustainedBusy(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	busy := func() LifecycleObservation { mono += time.Second; o := healthyObs(mono); o.UtilPct = 95; return o }
	m.Step(busy())
	st, _ := m.Step(busy())
	if st != StateBusy {
		t.Fatalf("sustained high util want BUSY, got %s", st)
	}
}

// DEGRADED on low stability score.
func TestLifecycle_DegradedOnLowStability(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	mono += time.Second
	o := healthyObs(mono); o.StabilityScore = 0.3
	st, _ := m.Step(o)
	if st != StateDegraded {
		t.Fatalf("low stability want DEGRADED, got %s", st)
	}
}

// Uncorrectable errors force DEGRADED even when probe OK.
func TestLifecycle_UncorrErrorsDegraded(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	mono += time.Second
	o := healthyObs(mono); o.NewUncorrErrors = 1
	st, _ := m.Step(o)
	if st != StateDegraded {
		t.Fatalf("uncorrectable errors want DEGRADED, got %s", st)
	}
}

// Threshold is configurable and enforced (e.g. OfflineFailures=5).
func TestLifecycle_ConfigurableThreshold(t *testing.T) {
	cfg := DefaultLifecycleConfig()
	cfg.OfflineFailures = 5
	m := NewLifecycleMachine(cfg)
	var mono time.Duration
	settle(m, &mono, 3)
	for i := 1; i <= 4; i++ {
		mono += time.Second
		if st, _ := m.Step(softFailObs(mono)); st != StateDegraded {
			t.Fatalf("failure %d want DEGRADED (threshold 5), got %s", i, st)
		}
	}
	mono += time.Second
	if st, _ := m.Step(softFailObs(mono)); st != StateOffline {
		t.Fatalf("failure 5 (threshold) want OFFLINE, got %s", st)
	}
	if m.Info().OfflineFailureThreshold != 5 {
		t.Fatalf("Info must expose threshold 5, got %d", m.Info().OfflineFailureThreshold)
	}
}

// Info exposes reason codes for audit.
func TestLifecycle_InfoReasonCodes(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	if rc := m.Info().ReasonCodes; len(rc) == 0 || rc[0] != ReasonHealthy {
		t.Fatalf("healthy reason code expected, got %v", rc)
	}
	mono += time.Second
	m.Step(softFailObs(mono))
	found := false
	for _, c := range m.Info().ReasonCodes {
		if c == ReasonGPUAccessProbeFail {
			found = true
		}
	}
	if !found {
		t.Fatalf("DEGRADED reason should include access-probe-fail, got %v", m.Info().ReasonCodes)
	}
}
