package core

import (
	"testing"
	"time"
)

func obs(ok, vis, acc bool, fails int, util, freeMem, score float64, mono time.Duration) LifecycleObservation {
	return LifecycleObservation{ProbeOK: ok, GPUVisible: vis, GPUAccessible: acc, ConsecutiveFailures: fails,
		UtilPct: util, UtilSupported: true, FreeMemRatio: freeMem, StabilityScore: score, Mono: mono}
}

func TestLifecycleReadyBusy(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	step := func(o LifecycleObservation) LifecycleState { mono += time.Second; o.Mono = mono; s, _ := m.Step(o); return s }
	// Healthy idle -> READY after hysteresis
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	s := step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	if s != StateReady { t.Fatalf("want READY got %s", s) }
	// High util -> BUSY (needs 2 confirms)
	step(obs(true, true, true, 0, 95, 0.9, 0.95, 0))
	s = step(obs(true, true, true, 0, 95, 0.9, 0.95, 0))
	if s != StateBusy { t.Fatalf("want BUSY got %s", s) }
}

func TestLifecycleOfflineImmediateAndRecovering(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	step := func(o LifecycleObservation) LifecycleState { mono += time.Second; o.Mono = mono; s, _ := m.Step(o); return s }
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	// 3 consecutive failures -> OFFLINE immediately
	step(obs(false, false, false, 3, 0, 0, 0.5, 0))
	if m.State() != StateOffline { t.Fatalf("want OFFLINE got %s", m.State()) }
	// Come back -> must pass through RECOVERING, not jump to READY
	s := step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	if s != StateRecovering { t.Fatalf("want RECOVERING got %s", s) }
	// Before hold elapses, stays RECOVERING
	s = step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	if s != StateRecovering { t.Fatalf("want still RECOVERING got %s", s) }
	// After hold (5s) -> promote
	for i := 0; i < 6; i++ { s = step(obs(true, true, true, 0, 5, 0.9, 0.95, 0)) }
	if s != StateReady { t.Fatalf("want READY after recovery hold got %s", s) }
	if !m.HasBeenOffline() { t.Fatal("should record offline history") }
}

func TestLifecycleDegradedOnLowScore(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	step := func(o LifecycleObservation) LifecycleState { mono += time.Second; o.Mono = mono; s, _ := m.Step(o); return s }
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	s := step(obs(true, true, true, 0, 5, 0.9, 0.3, 0)) // low stability
	if s != StateDegraded { t.Fatalf("want DEGRADED got %s", s) }
}

func TestLifecycleNoFlapping(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	transitions := 0
	step := func(o LifecycleObservation) { mono += time.Second; o.Mono = mono; _, tr := m.Step(o); if tr { transitions++ } }
	// settle to READY
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	base := transitions
	// Single-sample util spike should NOT flip to BUSY (needs 2 confirms)
	step(obs(true, true, true, 0, 95, 0.9, 0.95, 0)) // spike
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))  // back to idle
	step(obs(true, true, true, 0, 95, 0.9, 0.95, 0)) // spike
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))  // back to idle
	if transitions > base {
		t.Fatalf("flapping: single-sample spikes caused %d transitions", transitions-base)
	}
}

func TestLifecycleErrorForcesDegraded(t *testing.T) {
	m := NewLifecycleMachine(DefaultLifecycleConfig())
	var mono time.Duration
	step := func(o LifecycleObservation) LifecycleState { mono += time.Second; o.Mono = mono; s, _ := m.Step(o); return s }
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	step(obs(true, true, true, 0, 5, 0.9, 0.95, 0))
	o := obs(true, true, true, 0, 5, 0.9, 0.95, 0)
	o.NewUncorrErrors = 1
	s, _ := m.Step(o)
	if s != StateDegraded { t.Fatalf("uncorrectable error must force DEGRADED, got %s", s) }
}
