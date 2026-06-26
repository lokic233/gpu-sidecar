package router

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAtomicSnapshot_NoCrossGenerationMix proves a routing decision NEVER sees backend state from one
// generation mixed with a cache directory from another. We hammer Snapshot() reads against rapid
// atomic publishes where each generation has a self-consistent invariant: a backend's CacheConfidence
// equals (generation%2 ? 1 : 0) AND its directory entry equals the SAME parity. A torn read would
// show confidence and directory from different generations.
func TestAtomicSnapshot_NoCrossGenerationMix(t *testing.T) {
	reg := NewRegistry([]Backend{{ID: "b"}}, time.Hour) // long interval; we publish manually
	var stop atomic.Bool
	var wg sync.WaitGroup

	// publisher: alternate a fully self-consistent snapshot each generation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var gen uint64
		for !stop.Load() {
			gen++
			parity := int(gen % 2)
			st := BackendState{Backend: Backend{ID: "b"}, Reachable: true, RuntimeHealthy: true,
				LifecycleState: "READY", CacheObservationSupported: true,
				CacheConfidence: float64(parity)} // 0 or 1
			dir := map[string]map[string]int{"b": {"key": parity}} // 0 or 1 — must match confidence
			reg.snap.Store(&BackendSnapshot{Generation: gen, Backends: []BackendState{st},
				Timestamp: time.Now(), CacheDirectory: dir})
		}
	}()

	// readers: each read must be internally consistent (confidence parity == directory parity).
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20000; i++ {
				snap := reg.Snapshot()
				if len(snap.Backends) == 0 {
					continue
				}
				confParity := int(snap.Backends[0].CacheConfidence)
				dirParity := snap.LookupPrefixTokens("b", "key")
				if confParity != dirParity {
					t.Errorf("CROSS-GENERATION MIX: gen=%d conf-parity=%d dir-parity=%d",
						snap.Generation, confParity, dirParity)
					return
				}
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

// TestAtomicSnapshot_PolicyUsesSameGeneration proves the policy resolves locality from the SAME
// snapshot pointer passed to SelectBackend (never a mutable global).
func TestAtomicSnapshot_PolicyUsesSameGeneration(t *testing.T) {
	p := NewCacheAwarePolicy(DefaultCacheAwareConfig(), nil)
	a := mkBackend("a")
	a.CacheObservationSupported = true
	a.CacheConfidence = 1
	a.QueueDepth = 3
	b := mkBackend("b")
	b.CacheObservationSupported = true
	b.CacheConfidence = 1
	key := "k"
	// snapshot gen1: 'a' is hot for key
	snap1 := &BackendSnapshot{Generation: 1, Backends: []BackendState{a, b}, Timestamp: time.Now(),
		CacheDirectory: map[string]map[string]int{"a": {key: 1000}}}
	req := RequestFeatures{InputLenEst: 1000, RequestedOutput: 20, PrefixKeyHash: key, PrefixTokens: 1000, CacheEligible: true}
	if dec, _ := p.SelectBackend(req, snap1); dec.BackendID != "a" {
		t.Fatalf("gen1: 'a' is hot and should win despite small queue, got %s", dec.BackendID)
	}
	// snapshot gen2: directory moved hotness to 'b' (and 'a' no longer hot). Same policy instance.
	snap2 := &BackendSnapshot{Generation: 2, Backends: []BackendState{a, b}, Timestamp: time.Now(),
		CacheDirectory: map[string]map[string]int{"b": {key: 1000}}}
	if dec, _ := p.SelectBackend(req, snap2); dec.BackendID != "b" {
		t.Fatalf("gen2: hotness moved to 'b'; policy must use snap2's directory, got %s", dec.BackendID)
	}
}

// TestAtomicSnapshot_ResetEpochMatchesDirectory: reset epoch and directory are published together,
// so a reader cannot see a new epoch with an old directory.
func TestAtomicSnapshot_ResetEpochAndDirectoryShareSnapshot(t *testing.T) {
	reg := NewRegistry([]Backend{{ID: "b"}}, time.Hour)
	snap := &BackendSnapshot{Generation: 7, Timestamp: time.Now(),
		Backends:       []BackendState{{Backend: Backend{ID: "b"}, CacheResetEpoch: 3}},
		CacheDirectory: map[string]map[string]int{"b": {"k": 5}}}
	reg.snap.Store(snap)
	got := reg.Snapshot()
	if got.Generation != 7 || got.Backends[0].CacheResetEpoch != 3 || got.LookupPrefixTokens("b", "k") != 5 {
		t.Fatalf("epoch and directory must come from the same snapshot, got %+v", got)
	}
}
