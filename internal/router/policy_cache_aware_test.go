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

// snapOf builds an atomic routing snapshot with an embedded cache directory. dir maps
// backendID -> (prefixKeyHash -> READY matched tokens). The policy resolves locality from THIS
// snapshot (no mutable global / no cross-generation mix).
func snapOf(dir map[string]map[string]int, bs ...BackendState) *BackendSnapshot {
	return &BackendSnapshot{Generation: 1, Backends: bs, Timestamp: time.Now(), CacheDirectory: dir}
}

func TestCacheAware_NoPrefixReuse_ReducesToLoad(t *testing.T) {
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.QueueDepth = 0
	b := mkBackend("b")
	b.QueueDepth = 10
	dec, err := p.SelectBackend(RequestFeatures{InputLenEst: 100, RequestedOutput: 50}, snapOf(nil, a, b))
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackendID != "a" {
		t.Fatalf("expected lightly-loaded 'a' to win under no-prefix, got %s", dec.BackendID)
	}
}

func TestCacheAware_HotAndLightlyLoadedWins(t *testing.T) {
	key := "deadbeef"
	dir := map[string]map[string]int{"b": {key: 900}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	b.QueueDepth = 1
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec, err := p.SelectBackend(req, snapOf(dir, a, b))
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackendID != "b" {
		t.Fatalf("expected cache-hot lightly-loaded 'b' to win, got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_HotButOverloadedLoses(t *testing.T) {
	key := "deadbeef"
	dir := map[string]map[string]int{"b": {key: 1000}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	b.QueueDepth = 200
	b.RuntimeWaiting = 50
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec, err := p.SelectBackend(req, snapOf(dir, a, b))
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackendID != "a" {
		t.Fatalf("expected overloaded cache-hot 'b' to LOSE to idle cold 'a', got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_LowConfidenceIgnored(t *testing.T) {
	key := "deadbeef"
	dir := map[string]map[string]int{"b": {key: 1000}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 0.1 // below ConfidenceFloor -> locality ignored
	b.QueueDepth = 2
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec, _ := p.SelectBackend(req, snapOf(dir, a, b))
	if dec.BackendID != "a" {
		t.Fatalf("expected low-confidence locality ignored -> 'a' wins, got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_StaleIgnored(t *testing.T) {
	key := "deadbeef"
	dir := map[string]map[string]int{"b": {key: 1000}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 0 // stale
	a.QueueDepth = 0
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 0 // stale
	b.QueueDepth = 5
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec, _ := p.SelectBackend(req, snapOf(dir, a, b))
	if dec.BackendID != "a" {
		t.Fatalf("expected stale locality ignored -> least loaded 'a', got %s (%s)", dec.BackendID, dec.Reason)
	}
}

func TestCacheAware_UnsupportedNeverRealZero(t *testing.T) {
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	b := mkBackend("b")
	req := RequestFeatures{InputLenEst: 100, RequestedOutput: 50}
	dec, _ := p.SelectBackend(req, snapOf(nil, a, b))
	if dec.BackendID != "a" {
		t.Fatalf("expected deterministic tie-break 'a', got %s", dec.BackendID)
	}
	sb := p.ScoreBreakdown(req, snapOf(nil, a))
	if sb[0].StalenessPenaltyMs != 0 {
		t.Fatalf("unsupported backend must not incur staleness penalty, got %f", sb[0].StalenessPenaltyMs)
	}
}

func TestCacheAware_OrderIndependence(t *testing.T) {
	key := "deadbeef"
	dir := map[string]map[string]int{"b": {key: 900}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	dec1, _ := p.SelectBackend(req, snapOf(dir, a, b))
	dec2, _ := p.SelectBackend(req, snapOf(dir, b, a))
	if dec1.BackendID != dec2.BackendID {
		t.Fatalf("backend order changed the chosen logical backend: %s vs %s", dec1.BackendID, dec2.BackendID)
	}
}

func TestCacheAware_HeterogeneousProfilesRespected(t *testing.T) {
	// Two cold, equally-queued backends with DIFFERENT configured PROFILES (static capability).
	// The faster-profile backend wins. (NOT driven by live aggregate throughput — see P1 #6.)
	profiles := map[string]BackendProfile{
		"fast": {DecodeMsPerToken: 0.2},
		"slow": {DecodeMsPerToken: 2.0},
	}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), profiles)
	fast := mkBackend("fast")
	slow := mkBackend("slow")
	req := RequestFeatures{InputLenEst: 100, RequestedOutput: 500}
	dec, _ := p.SelectBackend(req, snapOf(nil, slow, fast))
	if dec.BackendID != "fast" {
		t.Fatalf("expected faster-profile backend to win, got %s (%s)", dec.BackendID, dec.Reason)
	}
}

// P1 #6: a backend that is BUSIER (higher live aggregate throughput AND growing backlog) must NOT
// become more attractive. Aggregate throughput is telemetry only; the busy backend's queue term
// makes it LOSE. This is the anti-feedback-loop guarantee.
func TestCacheAware_BusyAggregateThroughputDoesNotAttract(t *testing.T) {
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil) // no profiles -> identical fallback decode
	idle := mkBackend("idle")
	idle.QueueDepth = 0
	idle.RuntimeRunning = 0
	idle.GenTokensPerSec = 100 // low aggregate throughput simply because it is idle
	idle.ServiceRateSupported = true
	busy := mkBackend("busy")
	busy.QueueDepth = 20      // growing backlog
	busy.RuntimeRunning = 16  // large continuous batch
	busy.RuntimeWaiting = 8
	busy.GenTokensPerSec = 8000 // HIGH aggregate throughput *because* it is busy (batched)
	busy.ServiceRateSupported = true
	req := RequestFeatures{InputLenEst: 100, RequestedOutput: 200}
	dec, _ := p.SelectBackend(req, snapOf(nil, idle, busy))
	if dec.BackendID != "idle" {
		t.Fatalf("busy high-aggregate-throughput backend must NOT win; got %s (%s)", dec.BackendID, dec.Reason)
	}
	// and the analytical decode cost must be identical (profile fallback), proving aggregate
	// throughput is not used as per-request speed.
	sb := p.ScoreBreakdown(req, snapOf(nil, idle, busy))
	var idleDecode, busyDecode float64
	for _, c := range sb {
		if c.BackendID == "idle" {
			idleDecode = c.EstDecodeMs
		} else {
			busyDecode = c.EstDecodeMs
		}
	}
	if idleDecode != busyDecode {
		t.Fatalf("decode cost must NOT depend on live aggregate throughput; idle=%f busy=%f", idleDecode, busyDecode)
	}
	if !sb[0].ProfileFallback {
		t.Fatalf("expected profile_fallback=true when no profile configured")
	}
}

func TestCacheAware_ProfileFallbackRecorded(t *testing.T) {
	withProfile := NewCacheAwarePolicy(DefaultCacheAwareConfig(), map[string]BackendProfile{"a": {DecodeMsPerToken: 0.5}})
	a := mkBackend("a")
	sb := withProfile.ScoreBreakdown(RequestFeatures{InputLenEst: 10, RequestedOutput: 10}, snapOf(nil, a))
	if sb[0].ProfileFallback {
		t.Fatalf("expected profile_fallback=false when a profile exists")
	}
}

func TestCacheAware_CacheResetInvalidatesLocality(t *testing.T) {
	key := "deadbeef"
	dir := map[string]map[string]int{"b": {key: 1000}}
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	a.QueueDepth = 0
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	b.QueueDepth = 5
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 50, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	if dec, _ := p.SelectBackend(req, snapOf(dir, a, b)); dec.BackendID != "b" {
		t.Fatalf("precondition: hot 'b' should win, got %s", dec.BackendID)
	}
	// after reset: empty directory (new generation) -> locality gone -> 'a' (less loaded) wins
	if dec, _ := p.SelectBackend(req, snapOf(map[string]map[string]int{}, a, b)); dec.BackendID != "a" {
		t.Fatalf("after cache reset, locality gone -> 'a' should win, got %s", dec.BackendID)
	}
}

func TestCacheAffinityOnly_HerdsOntoHot(t *testing.T) {
	key := "deadbeef"
	dir := map[string]map[string]int{"b": {key: 500}}
	p := NewCacheAffinityOnlyPolicy()
	a := mkBackend("a")
	a.CacheObservationSupported = true
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.QueueDepth = 200 // heavily loaded but under max -> affinity-only ignores load
	req := RequestFeatures{InputLenEst: 1000, PrefixKeyHash: key, PrefixTokens: 500, CacheEligible: true}
	dec, _ := p.SelectBackend(req, snapOf(dir, a, b))
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
	dec, err := p.SelectBackend(RequestFeatures{InputLenEst: 10}, snapOf(nil, off, drain, ok))
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
	_, err := p.SelectBackend(RequestFeatures{}, snapOf(nil, off))
	if err != ErrNoEligibleBackend {
		t.Fatalf("expected ErrNoEligibleBackend, got %v", err)
	}
}

func TestServiceRate_DeltaNotCumulative(t *testing.T) {
	// The registry still computes a RATE from cumulative counters (telemetry), not the raw total.
	reg := NewRegistry([]Backend{{ID: "x"}}, time.Second)
	if r, sup := reg.serviceRate("x", 1000, true); sup || r != 0 {
		t.Fatalf("first sample must be unsupported rate, got r=%f sup=%v", r, sup)
	}
	time.Sleep(20 * time.Millisecond)
	r, sup := reg.serviceRate("x", 1100, true)
	if !sup {
		t.Fatalf("second sample should be supported")
	}
	if r <= 0 || r > 100000 {
		t.Fatalf("rate should be a sane delta/dt, got %f", r)
	}
	if r2, sup2 := reg.serviceRate("x", 5, true); !sup2 || r2 != 0 {
		t.Fatalf("counter reset must give 0 rate supported, got r=%f sup=%v", r2, sup2)
	}
}
