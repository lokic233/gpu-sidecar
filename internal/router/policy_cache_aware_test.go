package router

import (
	"testing"
	"time"
)

// mkBackend builds a BackendState with sensible eligible defaults.
func mkBackend(id string) BackendState {
	return BackendState{
		Backend:        Backend{ID: id},
		Reachable:      true,
		RuntimeHealthy: true,
		LifecycleState: "READY",
		QueueMax:       256,
		StabilityScore: 1.0,
	}
}

func snapOf(bs ...BackendState) *BackendSnapshot {
	return &BackendSnapshot{Backends: bs, Timestamp: time.Now()}
}

// staticLocator returns fixed matched-token counts for (backend, key) pairs.
type staticLocator struct{ m map[string]int }

func (s staticLocator) LookupPrefixTokens(backendID, keyHash string) int {
	return s.m[backendID+"|"+keyHash]
}

func TestCacheAware_NoPrefixReuse_ReducesToLoad(t *testing.T) {
	// No cache eligibility: behavior must reduce to the load-aware estimate (least loaded wins).
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.QueueDepth = 0
	b := mkBackend("b")
	b.QueueDepth = 10 // heavily queued
	dec, err := p.SelectBackend(RequestFeatures{InputLenEst: 100, RequestedOutput: 50}, snapOf(a, b))
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackendID != "a" {
		t.Fatalf("expected lightly-loaded 'a' to win under no-prefix, got %s", dec.BackendID)
	}
}

func TestCacheAware_HotAndLightlyLoadedWins(t *testing.T) {
	key := "deadbeef"
	loc := staticLocator{m: map[string]int{"b|" + key: 900}} // b is cache-hot for this key
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), loc)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	b.QueueDepth = 1 // slightly more loaded but cache-hot
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec, err := p.SelectBackend(req, snapOf(a, b))
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackendID != "b" {
		t.Fatalf("expected cache-hot lightly-loaded 'b' to win, got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_HotButOverloadedLoses(t *testing.T) {
	key := "deadbeef"
	loc := staticLocator{m: map[string]int{"b|" + key: 1000}} // b is fully cache-hot
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), loc)
	a := mkBackend("a") // cold but idle
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	b := mkBackend("b") // cache-hot but massively overloaded
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	b.QueueDepth = 200
	b.RuntimeWaiting = 50
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec, err := p.SelectBackend(req, snapOf(a, b))
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackendID != "a" {
		t.Fatalf("expected overloaded cache-hot 'b' to LOSE to idle cold 'a', got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_LowConfidenceIgnored(t *testing.T) {
	key := "deadbeef"
	loc := staticLocator{m: map[string]int{"b|" + key: 1000}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), loc)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 0.1 // below ConfidenceFloor -> locality ignored
	b.QueueDepth = 2        // slightly more loaded
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec, _ := p.SelectBackend(req, snapOf(a, b))
	if dec.BackendID != "a" {
		t.Fatalf("expected low-confidence locality ignored -> 'a' wins, got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_StaleIgnored(t *testing.T) {
	// stale modeled as confidence 0 with supported plane: locality ignored, staleness penalty applies
	// equally-ish; the lighter backend should win.
	key := "deadbeef"
	loc := staticLocator{m: map[string]int{"b|" + key: 1000}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), loc)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 0 // stale
	a.QueueDepth = 0
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 0 // stale
	b.QueueDepth = 5
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec, _ := p.SelectBackend(req, snapOf(a, b))
	if dec.BackendID != "a" {
		t.Fatalf("expected stale locality ignored -> least loaded 'a', got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_UnsupportedNeverRealZero(t *testing.T) {
	// A backend with cache observation UNSUPPORTED must not be treated as "0 matched but valid".
	// It should behave like the load-only estimate (no staleness penalty either, since not supported).
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a") // unsupported cache, idle
	b := mkBackend("b") // unsupported cache, idle, identical
	// identical load => tie broken deterministically by id (a < b)
	req := RequestFeatures{InputLenEst: 100, RequestedOutput: 50}
	dec, _ := p.SelectBackend(req, snapOf(a, b))
	if dec.BackendID != "a" {
		t.Fatalf("expected deterministic tie-break 'a', got %s", dec.BackendID)
	}
	// and a supported-but-stale backend should be penalized vs an unsupported idle one only by the
	// staleness term; verify unsupported backend does not incur a staleness penalty.
	sb := p.ScoreBreakdown(req, snapOf(a))
	if sb[0].StalenessPenaltyMs != 0 {
		t.Fatalf("unsupported backend must not incur staleness penalty, got %f", sb[0].StalenessPenaltyMs)
	}
}

func TestCacheAware_OrderIndependence(t *testing.T) {
	key := "deadbeef"
	loc := staticLocator{m: map[string]int{"b|" + key: 900}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), loc)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec1, _ := p.SelectBackend(req, snapOf(a, b))
	dec2, _ := p.SelectBackend(req, snapOf(b, a)) // reversed order
	if dec1.BackendID != dec2.BackendID {
		t.Fatalf("backend order changed the chosen logical backend: %s vs %s", dec1.BackendID, dec2.BackendID)
	}
}

func TestCacheAware_HeterogeneousServiceRates(t *testing.T) {
	// Two cold, equally-queued backends with DIFFERENT measured decode rates. The faster one wins
	// (different-capability backends are NOT given equal traffic by construction).
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	fast := mkBackend("fast")
	fast.ServiceRateSupported = true
	fast.GenTokensPerSec = 5000 // fast decode
	slow := mkBackend("slow")
	slow.ServiceRateSupported = true
	slow.GenTokensPerSec = 500 // slow decode
	req := RequestFeatures{InputLenEst: 100, RequestedOutput: 500}
	dec, _ := p.SelectBackend(req, snapOf(slow, fast))
	if dec.BackendID != "fast" {
		t.Fatalf("expected faster service rate to win, got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_CacheResetInvalidatesLocality(t *testing.T) {
	// Model a cache reset by flipping the locator to no-match (router would re-materialize empty
	// directory after the sidecar bumps reset epoch). The previously hot backend should no longer win.
	key := "deadbeef"
	hot := staticLocator{m: map[string]int{"b|" + key: 1000}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), hot)
	a := mkBackend("a") // idle, cold
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	a.QueueDepth = 0
	b := mkBackend("b") // loaded, but cache-hot (locality outweighs the modest queue)
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	b.QueueDepth = 5
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	if dec, _ := p.SelectBackend(req, snapOf(a, b)); dec.BackendID != "b" {
		t.Fatalf("precondition: hot 'b' should win, got %s", dec.BackendID)
	}
	// after reset: empty locator -> no locality, 'a' (less loaded) wins
	pReset := NewCacheAwarePolicy(DefaultCacheAwareConfig(), staticLocator{m: map[string]int{}})
	if dec, _ := pReset.SelectBackend(req, snapOf(a, b)); dec.BackendID != "a" {
		t.Fatalf("after cache reset, locality gone -> 'a' should win, got %s", dec.BackendID)
	}
}

func TestCacheAffinityOnly_HerdsOntoHot(t *testing.T) {
	key := "deadbeef"
	loc := staticLocator{m: map[string]int{"b|" + key: 500}}
	p := NewCacheAffinityOnlyPolicy(loc)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.QueueDepth = 200 // heavily loaded (but under max, still eligible) — affinity-only ignores load
	req := RequestFeatures{InputLenEst: 1000, PrefixKeyHash: key, PrefixTokens: 500, CacheEligible: true}
	dec, _ := p.SelectBackend(req, snapOf(a, b))
	if dec.BackendID != "b" {
		t.Fatalf("affinity-only must herd onto cache-hot 'b' regardless of load, got %s", dec.BackendID)
	}
}

func TestEligibility_OfflineDrainingExcluded(t *testing.T) {
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	off := mkBackend("off")
	off.LifecycleState = "OFFLINE"
	drain := mkBackend("drain")
	drain.LifecycleState = "DRAINING"
	ok := mkBackend("ok")
	dec, err := p.SelectBackend(RequestFeatures{InputLenEst: 10}, snapOf(off, drain, ok))
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackendID != "ok" {
		t.Fatalf("expected only eligible 'ok', got %s", dec.BackendID)
	}
}

func TestCacheAware_NoEligibleBackend(t *testing.T) {
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	off := mkBackend("off")
	off.RuntimeHealthy = false
	_, err := p.SelectBackend(RequestFeatures{}, snapOf(off))
	if err != ErrNoEligibleBackend {
		t.Fatalf("expected ErrNoEligibleBackend, got %v", err)
	}
}

func TestServiceRate_DeltaNotCumulative(t *testing.T) {
	// Verify the registry computes a RATE from cumulative counters, not the raw total.
	reg := NewRegistry([]Backend{{ID: "x"}}, time.Second)
	// first sample: no rate yet (need two)
	if r, sup := reg.serviceRate("x", 1000, true); sup || r != 0 {
		t.Fatalf("first sample must be unsupported rate, got r=%f sup=%v", r, sup)
	}
	time.Sleep(20 * time.Millisecond)
	// second sample: cumulative grew by 100 over ~dt -> positive finite rate, NOT 1100
	r, sup := reg.serviceRate("x", 1100, true)
	if !sup {
		t.Fatalf("second sample should be supported")
	}
	if r <= 0 || r > 100000 {
		t.Fatalf("rate should be a sane delta/dt, got %f", r)
	}
	// counter reset (current < prev) -> 0 rate but supported
	if r2, sup2 := reg.serviceRate("x", 5, true); !sup2 || r2 != 0 {
		t.Fatalf("counter reset must give 0 rate supported, got r=%f sup=%v", r2, sup2)
	}
}
