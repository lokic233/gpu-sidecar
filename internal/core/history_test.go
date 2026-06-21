package core

import (
	"testing"
	"time"
)

func TestRingBounded(t *testing.T) {
	r := newRing[int](3)
	for i := 0; i < 10; i++ { r.push(i) }
	items := r.items()
	if len(items) != 3 { t.Fatalf("ring not bounded: %d", len(items)) }
	if items[0] != 7 || items[2] != 9 { t.Fatalf("ring order wrong: %v", items) }
}

func TestHistoryAvailabilityAndPercentiles(t *testing.T) {
	h := NewDeviceHistory(100, 100, 100, 60)
	wall := time.Now()
	var mono time.Duration
	// 8 ok, 2 fail
	lats := []float64{5, 6, 7, 8, 9, 10, 11, 12}
	for _, l := range lats { mono += time.Second; h.RecordProbe(true, l, mono, wall) }
	mono += time.Second; h.RecordProbe(false, 0, mono, wall)
	mono += time.Second; h.RecordProbe(false, 0, mono, wall)
	rel := h.Snapshot()
	if rel.ConsecutiveFailures != 2 { t.Fatalf("want 2 consecutive failures got %d", rel.ConsecutiveFailures) }
	if rel.RecentAvailability < 0.79 || rel.RecentAvailability > 0.81 { t.Fatalf("availability ~0.8 got %v", rel.RecentAvailability) }
	if rel.ProbeLatencyP50Ms <= 0 || rel.ProbeLatencyP95Ms < rel.ProbeLatencyP50Ms { t.Fatalf("bad percentiles p50=%v p95=%v", rel.ProbeLatencyP50Ms, rel.ProbeLatencyP95Ms) }
}

func TestHistoryUncorrDelta(t *testing.T) {
	h := NewDeviceHistory(10, 10, 10, 60)
	if d := h.ObserveUncorr(5, true); d != 0 { t.Fatalf("first observation delta should be 0, got %d", d) }
	if d := h.ObserveUncorr(5, true); d != 0 { t.Fatalf("no change delta 0, got %d", d) }
	if d := h.ObserveUncorr(8, true); d != 3 { t.Fatalf("delta should be 3, got %d", d) }
	if d := h.ObserveUncorr(8, false); d != 0 { t.Fatalf("unsupported delta 0, got %d", d) }
}

func TestHistoryDisconnectWindow(t *testing.T) {
	h := NewDeviceHistory(10, 10, 10, 10) // 10s window
	h.MarkDisconnect(1 * time.Second)
	h.MarkDisconnect(2 * time.Second)
	if c := h.DisconnectsInWindow(5 * time.Second); c != 2 { t.Fatalf("want 2 in window got %d", c) }
	if c := h.DisconnectsInWindow(20 * time.Second); c != 0 { t.Fatalf("want 0 (aged out) got %d", c) }
}

func TestNonMonotonicSafe(t *testing.T) {
	// Feed decreasing wall clock; durations use monotonic mono param so must not break.
	h := NewDeviceHistory(10, 10, 10, 60)
	future := time.Now()
	past := future.Add(-time.Hour)
	h.RecordProbe(true, 5, 1*time.Second, future)
	h.RecordProbe(true, 6, 2*time.Second, past) // wall went backwards
	rel := h.Snapshot()
	if rel.RecentAvailability != 1.0 { t.Fatalf("clock skew broke availability: %v", rel.RecentAvailability) }
}
