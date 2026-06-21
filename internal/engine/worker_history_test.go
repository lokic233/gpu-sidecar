package engine

import (
	"testing"
	"time"
)

func TestWorkerEventLog_BoundedBySize(t *testing.T) {
	w := newWorkerEventLog(3600, 100) // huge window, max 100 entries
	var now time.Duration
	for i := 0; i < 10000; i++ {
		now += time.Second
		w.recordDisappearance(now)
	}
	if len(w.disappearances) > 100 {
		t.Fatalf("disappearance history must be bounded to 100, got %d", len(w.disappearances))
	}
	// most recent must be retained
	last := w.disappearances[len(w.disappearances)-1]
	if last != now {
		t.Fatalf("most recent event must be retained, got %v want %v", last, now)
	}
}

func TestWorkerEventLog_BoundedByAge(t *testing.T) {
	w := newWorkerEventLog(60, 100000) // 60s window, large size cap
	var now time.Duration
	// 200 events 1s apart => only last ~60 within window
	for i := 0; i < 200; i++ {
		now += time.Second
		w.recordDisappearance(now)
	}
	c := w.disappearancesInWindow(now)
	if c > 61 {
		t.Fatalf("only ~60s of events should remain in window, got %d", c)
	}
	if c < 59 {
		t.Fatalf("recent events should be retained (~60), got %d", c)
	}
}

func TestWorkerEventLog_OldEventsDontAffectScoring(t *testing.T) {
	w := newWorkerEventLog(60, 100000)
	var now time.Duration
	// old burst
	for i := 0; i < 50; i++ {
		now += time.Second
		w.recordDisappearance(now)
	}
	// advance 10 minutes with no events
	now += 600 * time.Second
	if c := w.disappearancesInWindow(now); c != 0 {
		t.Fatalf("old events must age out of the window, got %d", c)
	}
}

func TestWorkerEventLog_RecentRetained(t *testing.T) {
	w := newWorkerEventLog(60, 100000)
	var now time.Duration
	now += 5 * time.Second
	w.recordDisappearance(now)
	now += 5 * time.Second
	w.recordDisappearance(now)
	if c := w.disappearancesInWindow(now); c != 2 {
		t.Fatalf("recent events must be retained, got %d", c)
	}
}

func TestWorkerEventLog_RapidRestartDetection(t *testing.T) {
	w := newWorkerEventLog(120, 1000)
	var now time.Duration
	// disappear then reappear 3s later (within 10s rapid threshold) => 1 rapid restart
	now += 10 * time.Second
	w.recordDisappearance(now)
	now += 3 * time.Second
	w.recordAppearance(now)
	if r := w.rapidRestartEvents(now, 10.0); r != 1 {
		t.Fatalf("expected 1 rapid restart, got %d", r)
	}
	// a slow restart (60s later) is NOT rapid
	w2 := newWorkerEventLog(120, 1000)
	var now2 time.Duration
	now2 += 10 * time.Second
	w2.recordDisappearance(now2)
	now2 += 60 * time.Second
	w2.recordAppearance(now2)
	if r := w2.rapidRestartEvents(now2, 10.0); r != 0 {
		t.Fatalf("slow restart must not count as rapid, got %d", r)
	}
}

func TestWorkerEventLog_NoOrderingErrors(t *testing.T) {
	w := newWorkerEventLog(3600, 50)
	var now time.Duration
	for i := 0; i < 1000; i++ {
		now += time.Second
		w.recordDisappearance(now)
		// verify monotonic ordering preserved
		for j := 1; j < len(w.disappearances); j++ {
			if w.disappearances[j] < w.disappearances[j-1] {
				t.Fatalf("ordering error at iteration %d", i)
			}
		}
	}
}
