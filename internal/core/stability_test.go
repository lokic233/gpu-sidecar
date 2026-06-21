package core

import (
	"testing"
	"time"
)

func TestStabilityDecayFastRecoverSlow(t *testing.T) {
	c := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	// Start healthy
	healthy := StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1, LatencyP50Ms: 5, LatencyP95Ms: 6}
	for i := 0; i < 10; i++ {
		c.Update(healthy, now)
	}
	if c.Score() < 0.9 {
		t.Fatalf("expected high score when healthy, got %v", c.Score())
	}
	// Inject instability: failures + disconnects
	bad := StabilityInputs{RecentAvailability: 0.2, ConsecutiveFailures: 4, DisconnectsInWindow: 3, ThroughputVariance: -1}
	before := c.Score()
	c.Update(bad, now)
	after := c.Score()
	if after >= before {
		t.Fatalf("score should drop on instability: before=%v after=%v", before, after)
	}
	dropOneStep := before - after
	// One healthy probe must NOT erase the instability (memory).
	scoreAfterOneGood := c.Update(healthy, now).Score
	if scoreAfterOneGood > after+0.2 {
		t.Fatalf("one good probe erased instability too fast: %v -> %v", after, scoreAfterOneGood)
	}
	// Recovery must be slower than the drop.
	c2 := NewStabilityCalc(DefaultStabilityConfig())
	for i := 0; i < 10; i++ { c2.Update(healthy, now) }
	c2.Update(bad, now)
	low := c2.Score()
	c2.Update(healthy, now)
	recoverOneStep := c2.Score() - low
	if recoverOneStep >= dropOneStep {
		t.Fatalf("recovery step (%v) should be slower than drop step (%v)", recoverOneStep, dropOneStep)
	}
	t.Logf("drop=%.4f recover=%.4f (recovery slower ✓)", dropOneStep, recoverOneStep)
}

func TestStabilityComponentsExposed(t *testing.T) {
	c := NewStabilityCalc(DefaultStabilityConfig())
	s := c.Update(StabilityInputs{RecentAvailability: 0.9, ThroughputVariance: -1}, time.Now())
	for _, k := range []string{"availability", "failures", "disconnect", "recovery", "errors", "latency", "throughput", "instantaneous", "smoothed"} {
		if _, ok := s.Components[k]; !ok {
			t.Fatalf("missing component %q (must be auditable)", k)
		}
	}
}

func TestStabilityErrorPenalty(t *testing.T) {
	c := NewStabilityCalc(DefaultStabilityConfig())
	now := time.Now()
	clean := c.Update(StabilityInputs{RecentAvailability: 1, ThroughputVariance: -1}, now).Score
	c2 := NewStabilityCalc(DefaultStabilityConfig())
	withErr := c2.Update(StabilityInputs{RecentAvailability: 1, ErrorCountDelta: 2, ThroughputVariance: -1}, now).Score
	if withErr >= clean {
		t.Fatalf("uncorrectable errors must lower score: clean=%v err=%v", clean, withErr)
	}
}

func TestStabilityClampsBadInput(t *testing.T) {
	c := NewStabilityCalc(DefaultStabilityConfig())
	s := c.Update(StabilityInputs{RecentAvailability: 99, LatencyP50Ms: 0, LatencyP95Ms: 0, ThroughputVariance: -1}, time.Now())
	if s.Score < 0 || s.Score > 1 {
		t.Fatalf("score out of [0,1]: %v", s.Score)
	}
}
