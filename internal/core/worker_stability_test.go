package core

import (
	"testing"
	"time"
)

func baselineScore(t *testing.T) float64 {
	c := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	in := StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1, LatencyP50Ms: 5, LatencyP95Ms: 6}
	var s float64
	for i := 0; i < 10; i++ {
		s = c.Update(in, now).Score
	}
	return s
}

// One unknown disappearance must NOT reduce stability (neutral by default).
func TestStability_UnknownDisappearanceNeutral(t *testing.T) {
	base := baselineScore(t)
	c := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	in := StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1, LatencyP50Ms: 5, LatencyP95Ms: 6}
	for i := 0; i < 10; i++ {
		c.Update(in, now)
	}
	// inject a neutral disappearance
	in.WorkerDisappearancesObserved = 1
	s := c.Update(in, now).Score
	if s < base-1e-9 {
		t.Fatalf("unknown disappearance must be neutral: base=%v after=%v", base, s)
	}
}

// Many unknown disappearances (graceful churn) still neutral.
func TestStability_ManyUnknownDisappearancesNeutral(t *testing.T) {
	c := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	in := StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1, LatencyP50Ms: 5, LatencyP95Ms: 6}
	for i := 0; i < 10; i++ {
		c.Update(in, now)
	}
	before := c.Score()
	in.WorkerDisappearancesObserved = 20 // lots of graceful scale-down
	s := c.Update(in, now).Score
	if s < before-1e-9 {
		t.Fatalf("20 unknown disappearances must remain neutral: before=%v after=%v", before, s)
	}
}

// Confirmed abnormal exit DOES reduce stability.
func TestStability_ConfirmedAbnormalReducesScore(t *testing.T) {
	c := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	in := StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1, LatencyP50Ms: 5, LatencyP95Ms: 6}
	for i := 0; i < 10; i++ {
		c.Update(in, now)
	}
	before := c.Score()
	in.ConfirmedAbnormalWorkerExits = 2
	s := c.Update(in, now).Score
	if s >= before {
		t.Fatalf("confirmed abnormal exits must reduce score: before=%v after=%v", before, s)
	}
}

// Confirmed OOM reduces stability (strong).
func TestStability_ConfirmedOOMReducesScore(t *testing.T) {
	c := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	in := StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1, LatencyP50Ms: 5, LatencyP95Ms: 6}
	for i := 0; i < 10; i++ {
		c.Update(in, now)
	}
	before := c.Score()
	in.ConfirmedOOMEvents = 1
	s := c.Update(in, now).Score
	if s >= before {
		t.Fatalf("confirmed OOM must reduce score: before=%v after=%v", before, s)
	}
}

// Rapid restart loop reduces stability.
func TestStability_RapidRestartReducesScore(t *testing.T) {
	c := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	in := StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1, LatencyP50Ms: 5, LatencyP95Ms: 6}
	for i := 0; i < 10; i++ {
		c.Update(in, now)
	}
	before := c.Score()
	in.RapidRestartEvents = 3
	s := c.Update(in, now).Score
	if s >= before {
		t.Fatalf("rapid restart loop must reduce score: before=%v after=%v", before, s)
	}
}

// Neutral disappearance + a confirmed abnormal exit: only the confirmed part should drive the penalty.
func TestStability_NeutralPlusConfirmed(t *testing.T) {
	c1 := NewStabilityCalc(DefaultStabilityConfig())
	c2 := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	in := StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1, LatencyP50Ms: 5, LatencyP95Ms: 6}
	for i := 0; i < 10; i++ {
		c1.Update(in, now)
		c2.Update(in, now)
	}
	// c1: confirmed only; c2: confirmed + many neutral disappearances
	in1 := in; in1.ConfirmedAbnormalWorkerExits = 1
	in2 := in; in2.ConfirmedAbnormalWorkerExits = 1; in2.WorkerDisappearancesObserved = 15
	s1 := c1.Update(in1, now).Score
	s2 := c2.Update(in2, now).Score
	if absf(s1-s2) > 1e-9 {
		t.Fatalf("neutral disappearances must not change the score beyond the confirmed penalty: s1=%v s2=%v", s1, s2)
	}
}

func absf(x float64) float64 { if x < 0 { return -x }; return x }
