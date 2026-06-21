package core

import (
	"testing"
	"time"
)

// lowStabilityObs: healthy probe but stability below DegradedScore (drives classifyHealthy->DEGRADED).
func lowStabilityObs(mono time.Duration) LifecycleObservation {
	o := healthyObs(mono)
	o.StabilityScore = 0.30
	return o
}
func busyObs(mono time.Duration) LifecycleObservation {
	o := healthyObs(mono)
	o.UtilPct = 95
	return o
}

// REQUIRED SEQUENCE 1: OFFLINE -> RECOVERING -> low stability -> must stay RECOVERING (not bypass)
// -> healthy streak below threshold -> RECOVERING -> hold+streak satisfied -> READY
func TestRecoveryLatch_LowStabilityDoesNotBypass(t *testing.T) {
	cfg := DefaultLifecycleConfig() // RecoveringHoldSec=5, RecoveryStreak=3
	m := NewLifecycleMachine(cfg)
	var mono time.Duration
	settle(m, &mono, 3)
	// to OFFLINE
	mono += time.Second
	m.Step(hardFailObs(mono))
	if m.State() != StateOffline {
		t.Fatalf("setup OFFLINE failed: %s", m.State())
	}
	// returning -> RECOVERING
	mono += time.Second
	if st, _ := m.Step(healthyObs(mono)); st != StateRecovering {
		t.Fatalf("want RECOVERING, got %s", st)
	}
	// low stability during recovery -> MUST remain RECOVERING (latched), not DEGRADED
	mono += time.Second
	st, _ := m.Step(lowStabilityObs(mono))
	if st != StateRecovering {
		t.Fatalf("low stability during recovery MUST stay RECOVERING (latched), got %s", st)
	}
	// a single healthy probe must NOT promote to READY (would be the bypass)
	mono += time.Second
	st, _ = m.Step(healthyObs(mono))
	if st == StateReady {
		t.Fatalf("BYPASS DETECTED: promoted to READY without hold+streak, got %s", st)
	}
	// satisfy hold + streak
	for i := 0; i < 6; i++ {
		mono += 2 * time.Second
		st, _ = m.Step(healthyObs(mono))
	}
	if st != StateReady {
		t.Fatalf("want READY after hold+streak, got %s", st)
	}
}

// REQUIRED SEQUENCE 2: OFFLINE -> RECOVERING -> BUSY util -> stay RECOVERING -> util falls -> READY
func TestRecoveryLatch_BusyDoesNotBypass(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	mono += time.Second
	m.Step(hardFailObs(mono))
	mono += time.Second
	m.Step(healthyObs(mono)) // RECOVERING
	// high utilization during recovery -> must stay RECOVERING (not jump to BUSY then READY)
	mono += time.Second
	st, _ := m.Step(busyObs(mono))
	if st != StateRecovering {
		t.Fatalf("BUSY-level util during recovery must stay RECOVERING, got %s", st)
	}
	// util falls, satisfy hold+streak
	for i := 0; i < 6; i++ {
		mono += 2 * time.Second
		st, _ = m.Step(healthyObs(mono))
	}
	if st != StateReady {
		t.Fatalf("want READY after recovery, got %s", st)
	}
}

// REQUIRED SEQUENCE 3: OFFLINE -> RECOVERING -> soft failures reach threshold -> OFFLINE
func TestRecoveryLatch_SoftFailuresReturnOffline(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	mono += time.Second
	m.Step(hardFailObs(mono))
	mono += time.Second
	if st, _ := m.Step(healthyObs(mono)); st != StateRecovering {
		t.Fatalf("want RECOVERING, got %s", st)
	}
	// soft failures during recovery
	for i := 0; i < 3; i++ {
		mono += time.Second
		m.Step(softFailObs(mono))
	}
	if m.State() != StateOffline {
		t.Fatalf("soft failures reaching threshold during recovery must return OFFLINE, got %s", m.State())
	}
}

// REQUIRED SEQUENCE 4: OFFLINE -> RECOVERING -> hard disappearance -> OFFLINE immediately
func TestRecoveryLatch_HardFailureImmediateOffline(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	mono += time.Second
	m.Step(hardFailObs(mono))
	mono += time.Second
	m.Step(healthyObs(mono)) // RECOVERING
	mono += time.Second
	if st, _ := m.Step(hardFailObs(mono)); st != StateOffline {
		t.Fatalf("hard failure during recovery must go OFFLINE immediately, got %s", st)
	}
}

// Prove the recovery latch resets healthy streak when interrupted by a failure.
func TestRecoveryLatch_StreakResetsOnInterruption(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	settle(m, &mono, 3)
	mono += time.Second
	m.Step(hardFailObs(mono))
	mono += time.Second
	m.Step(healthyObs(mono)) // RECOVERING
	// build partial streak
	mono += time.Second
	m.Step(healthyObs(mono))
	// interrupt with a soft failure (below threshold)
	mono += time.Second
	m.Step(softFailObs(mono))
	// must still be in recovery (latched), streak reset
	if m.State() != StateRecovering {
		t.Fatalf("after soft-fail interruption during recovery, expected RECOVERING (latched), got %s", m.State())
	}
	if !m.Info().RecoveryLatched {
		t.Fatal("recovery must remain latched after a soft-failure interruption below threshold")
	}
	if m.Info().RecoveryHealthyStreak != 0 {
		t.Fatalf("recovery healthy streak must reset after interruption, got %d", m.Info().RecoveryHealthyStreak)
	}
}
