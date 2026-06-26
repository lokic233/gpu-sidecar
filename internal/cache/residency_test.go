package cache

import (
	"sync"
	"testing"
	"time"
)

func TestResidency_AbsentToWarmingToReady(t *testing.T) {
	r := NewResidency(100, time.Hour)
	if st, _ := r.Lookup("k"); st != StateAbsent {
		t.Fatalf("expected ABSENT initially, got %s", st)
	}
	r.BeginWarm("k", 200)
	if st, tok := r.Lookup("k"); st != StateWarming || tok != 0 {
		t.Fatalf("expected WARMING with 0 reusable tokens, got %s/%d", st, tok)
	}
	r.MarkReady("k")
	if st, tok := r.Lookup("k"); st != StateReady || tok != 200 {
		t.Fatalf("expected READY/200, got %s/%d", st, tok)
	}
}

func TestResidency_WarmingNotReusable(t *testing.T) {
	r := NewResidency(100, time.Hour)
	r.BeginWarm("k", 200)
	if _, tok := r.Lookup("k"); tok != 0 {
		t.Fatalf("WARMING must report 0 reusable tokens, got %d", tok)
	}
	if len(r.ReadyDirectory(100)) != 0 {
		t.Fatalf("WARMING must not be in the ready directory")
	}
}

func TestResidency_AbortReturnsToAbsent(t *testing.T) {
	r := NewResidency(100, time.Hour)
	r.BeginWarm("k", 200)
	r.AbortWarm("k")
	if st, _ := r.Lookup("k"); st != StateAbsent {
		t.Fatalf("expected ABSENT after sole warmer aborts, got %s", st)
	}
}

func TestResidency_ConcurrentWarmersOneAbortsOneReadies(t *testing.T) {
	r := NewResidency(100, time.Hour)
	r.BeginWarm("k", 200)
	r.BeginWarm("k", 200) // two in-flight warmers
	r.AbortWarm("k")      // first fails
	if st, _ := r.Lookup("k"); st == StateReady {
		t.Fatalf("must not be READY while a warmer is still in flight")
	}
	if st, _ := r.Lookup("k"); st != StateWarming {
		t.Fatalf("expected still WARMING, got %s", st)
	}
	r.MarkReady("k") // second succeeds
	if st, tok := r.Lookup("k"); st != StateReady || tok != 200 {
		t.Fatalf("expected READY/200 after surviving warmer, got %s/%d", st, tok)
	}
}

func TestResidency_MarkReadyOnAbsentIsFalseReady(t *testing.T) {
	r := NewResidency(100, time.Hour)
	// a stale readiness signal for a key that was never warming (or already reset) must NOT resurrect
	r.MarkReady("ghost")
	if st, _ := r.Lookup("ghost"); st != StateAbsent {
		t.Fatalf("MarkReady on ABSENT must not create a READY entry, got %s", st)
	}
	if r.Stats().FalseReady != 1 {
		t.Fatalf("expected 1 false-ready, got %d", r.Stats().FalseReady)
	}
}

func TestResidency_DuplicateMarkReadyIdempotent(t *testing.T) {
	r := NewResidency(100, time.Hour)
	r.BeginWarm("k", 100)
	r.MarkReady("k")
	r.MarkReady("k") // duplicate terminal — idempotent
	st, tok := r.Lookup("k")
	if st != StateReady || tok != 100 {
		t.Fatalf("duplicate MarkReady must be idempotent, got %s/%d", st, tok)
	}
}

func TestResidency_ResetClearsWarmingAndReady(t *testing.T) {
	r := NewResidency(100, time.Hour)
	r.BeginWarm("warm", 100)
	r.BeginWarm("ready", 100)
	r.MarkReady("ready")
	ep := r.ResetEpoch()
	r.Reset("runtime_restart")
	if r.ResetEpoch() != ep+1 {
		t.Fatalf("reset must bump epoch")
	}
	if st, _ := r.Lookup("ready"); st != StateAbsent {
		t.Fatalf("reset must clear READY, got %s", st)
	}
	if st, _ := r.Lookup("warm"); st != StateAbsent {
		t.Fatalf("reset must clear WARMING, got %s", st)
	}
}

func TestResidency_TTLExpiry(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	r := NewResidency(100, 5*time.Second).WithClock(clk.now)
	r.BeginWarm("k", 100)
	r.MarkReady("k")
	clk.advance(6 * time.Second)
	if st, _ := r.Lookup("k"); st != StateAbsent {
		t.Fatalf("expected TTL-expired READY -> ABSENT, got %s", st)
	}
}

func TestResidency_AbortAfterReadyDoesNotUnready(t *testing.T) {
	// a concurrent request readied the key; a late abort of another attempt must not un-ready it.
	r := NewResidency(100, time.Hour)
	r.BeginWarm("k", 100)
	r.BeginWarm("k", 100)
	r.MarkReady("k") // readied by one request
	r.AbortWarm("k") // the other attempt aborts
	if st, _ := r.Lookup("k"); st != StateReady {
		t.Fatalf("abort after ready must not un-ready, got %s", st)
	}
}

func TestResidency_RaceSafety(t *testing.T) {
	r := NewResidency(10000, time.Hour)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				k := keyN((g*2000 + i) % 256)
				switch i % 5 {
				case 0:
					r.BeginWarm(k, 64)
				case 1:
					r.MarkReady(k)
				case 2:
					r.AbortWarm(k)
				case 3:
					r.Lookup(k)
				default:
					r.Stats()
				}
			}
		}(g)
	}
	wg.Wait()
	_ = r.ReadyDirectory(100) // must not panic / race (run with -race)
}
