package cache

import (
	"sync"
	"testing"
	"time"
)

// fixedClock is a controllable clock for deterministic TTL/staleness tests.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fixedClock { return &fixedClock{t: t} }
func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestIndex(clk *fixedClock, cfg IndexConfig) *Index {
	idx := NewIndex(cfg)
	idx.WithClock(clk.now)
	return idx
}

func TestIndex_StoreAndLookup(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Minute, StaleAfter: 10 * time.Second})
	idx.ApplyStore(1, "k1", "", 16, 16)
	tok, conf, age := idx.LookupKey("k1")
	if tok != 16 {
		t.Fatalf("expected 16 matched tokens, got %d", tok)
	}
	if conf <= 0 {
		t.Fatalf("expected positive confidence, got %f", conf)
	}
	if age < 0 {
		t.Fatalf("expected non-negative age, got %d", age)
	}
}

func TestIndex_RemoveEvent(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Minute, StaleAfter: 10 * time.Second})
	idx.ApplyStore(1, "k1", "", 16, 16)
	idx.ApplyRemove(2, "k1")
	tok, _, _ := idx.LookupKey("k1")
	if tok != 0 {
		t.Fatalf("expected 0 tokens after remove, got %d", tok)
	}
}

func TestIndex_AllClear(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Minute, StaleAfter: 10 * time.Second})
	idx.ApplyStore(1, "k1", "", 16, 16)
	idx.ApplyStore(2, "k2", "", 16, 16)
	epochBefore := idx.ResetEpoch()
	idx.ApplyClear(3)
	if got := idx.ResetEpoch(); got != epochBefore+1 {
		t.Fatalf("expected reset epoch bump, got %d (was %d)", got, epochBefore)
	}
	if tok, _, _ := idx.LookupKey("k1"); tok != 0 {
		t.Fatalf("expected cleared, got %d", tok)
	}
}

func TestIndex_DuplicateEvent(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Minute, StaleAfter: 10 * time.Second})
	idx.ApplyStore(1, "k1", "", 16, 16)
	idx.ApplyStore(1, "k1", "", 16, 16) // exact duplicate seq
	s := idx.SnapshotMeta()
	if s.DuplicateEventsTotal != 1 {
		t.Fatalf("expected 1 duplicate, got %d", s.DuplicateEventsTotal)
	}
	if s.IndexEntries != 1 {
		t.Fatalf("expected 1 entry, got %d", s.IndexEntries)
	}
}

func TestIndex_OutOfOrderEvent(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Minute, StaleAfter: 10 * time.Second})
	idx.ApplyStore(5, "k5", "", 16, 16)
	idx.ApplyStore(3, "k3", "", 16, 16) // older seq arrives late
	s := idx.SnapshotMeta()
	if s.OutOfOrderEventsTotal != 1 {
		t.Fatalf("expected 1 out-of-order, got %d", s.OutOfOrderEventsTotal)
	}
	// late store still applied (idempotent, useful hint)
	if tok, _, _ := idx.LookupKey("k3"); tok != 16 {
		t.Fatalf("expected late store applied, got %d", tok)
	}
}

func TestIndex_SequenceGap(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Minute, StaleAfter: 10 * time.Second})
	idx.ApplyStore(1, "k1", "", 16, 16)
	idx.ApplyStore(5, "k5", "", 16, 16) // gap: missed 2,3,4
	s := idx.SnapshotMeta()
	if s.SequenceGapsTotal != 1 {
		t.Fatalf("expected 1 gap, got %d", s.SequenceGapsTotal)
	}
	// gap penalizes confidence
	_, conf, _ := idx.LookupKey("k5")
	if conf >= 1.0 {
		t.Fatalf("expected reduced confidence after gap, got %f", conf)
	}
}

func TestIndex_StaleSnapshot(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Minute, StaleAfter: 10 * time.Second})
	idx.ApplyStore(1, "k1", "", 16, 16)
	clk.advance(11 * time.Second) // exceed StaleAfter
	tok, conf, _ := idx.LookupKey("k1")
	if tok != 0 || conf != 0 {
		t.Fatalf("expected stale -> 0 tokens / 0 conf, got tok=%d conf=%f", tok, conf)
	}
	s := idx.SnapshotMeta()
	if s.Ready {
		t.Fatalf("expected not-ready when stale")
	}
	if s.StaleInvalidations == 0 {
		t.Fatalf("expected a stale invalidation count")
	}
}

func TestIndex_TTLExpiration(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	// EntryTTL < StaleAfter so an entry expires while index is still "fresh".
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: 5 * time.Second, StaleAfter: time.Hour})
	idx.ApplyStore(1, "k1", "", 16, 16)
	clk.advance(6 * time.Second)
	idx.ApplyStore(2, "k2", "", 16, 16) // keep index fresh
	if tok, _, _ := idx.LookupKey("k1"); tok != 0 {
		t.Fatalf("expected k1 TTL-expired, got %d", tok)
	}
	if tok, _, _ := idx.LookupKey("k2"); tok != 16 {
		t.Fatalf("expected k2 present, got %d", tok)
	}
}

func TestIndex_BoundedEviction(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 3, EntryTTL: time.Hour, StaleAfter: time.Hour})
	for i := 0; i < 10; i++ {
		clk.advance(time.Millisecond)
		idx.ApplyStore(int64(i+1), keyN(i), "", 16, 16)
	}
	if idx.Len() > 3 {
		t.Fatalf("expected <=3 entries (bounded), got %d", idx.Len())
	}
	// oldest keys evicted; newest present
	if tok, _, _ := idx.LookupKey(keyN(9)); tok != 16 {
		t.Fatalf("expected newest key present, got %d", tok)
	}
	if tok, _, _ := idx.LookupKey(keyN(0)); tok != 0 {
		t.Fatalf("expected oldest key evicted, got %d", tok)
	}
}

func TestIndex_RuntimeReset(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour})
	idx.ApplyStore(10, "k1", "", 16, 16)
	epochBefore := idx.ResetEpoch()
	idx.Reset("runtime_restart")
	if idx.ResetEpoch() != epochBefore+1 {
		t.Fatalf("expected reset epoch bump")
	}
	if tok, _, _ := idx.LookupKey("k1"); tok != 0 {
		t.Fatalf("expected wiped after reset, got %d", tok)
	}
	// after reset, a fresh low seq must NOT be a false gap
	idx.ApplyStore(1, "k2", "", 16, 16)
	if s := idx.SnapshotMeta(); s.SequenceGapsTotal != 0 {
		t.Fatalf("expected no gap after reset+reseed, got %d", s.SequenceGapsTotal)
	}
}

func TestIndex_ConcurrencySafety(t *testing.T) {
	idx := NewIndex(IndexConfig{MaxEntries: 10000, EntryTTL: time.Hour, StaleAfter: time.Hour})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				seq := int64(g*2000 + i + 1)
				switch i % 4 {
				case 0:
					idx.ApplyStore(seq, keyN(i), "", 16, 16)
				case 1:
					idx.ApplyRemove(seq, keyN(i-1))
				case 2:
					idx.LookupKey(keyN(i))
				default:
					idx.SnapshotMeta()
				}
			}
		}(g)
	}
	wg.Wait()
	_ = idx.SnapshotMeta() // must not panic / race (run with -race)
}

func TestIndex_Directory(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour})
	idx.ApplyStore(1, "k1", "", 16, 16)
	idx.ApplyStore(2, "k2", "", 32, 16)
	idx.ApplyRemove(3, "k1")
	d := idx.Directory(100)
	if _, ok := d["k1"]; ok {
		t.Fatalf("removed key should not be in directory")
	}
	if d["k2"] != 32 {
		t.Fatalf("expected k2=32 in directory, got %d", d["k2"])
	}
}

func TestIndex_DirectoryStaleEmpty(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: 5 * time.Second})
	idx.ApplyStore(1, "k1", "", 16, 16)
	clk.advance(6 * time.Second)
	if d := idx.Directory(100); len(d) != 0 {
		t.Fatalf("expected empty directory when stale, got %d entries", len(d))
	}
}

func keyN(i int) string {
	const hex = "0123456789abcdef"
	b := []byte("key-0000")
	b[4] = hex[(i>>12)&0xf]
	b[5] = hex[(i>>8)&0xf]
	b[6] = hex[(i>>4)&0xf]
	b[7] = hex[i&0xf]
	return string(b)
}


// --- Round-5 hardening: event ordering + trust-state tests ---

func TestIndex_OldRemoveCannotOverrideNewerStore(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour})
	// newer store at seq 10
	idx.ApplyStore(10, "k", "", 16, 16)
	// an OLD remove (seq 5) arrives late — must NOT delete the newer store
	idx.ApplyRemove(5, "k")
	if tok, _, _ := idx.LookupKey("k"); tok != 16 {
		t.Fatalf("old remove must not delete newer store, got %d", tok)
	}
}

func TestIndex_OldStoreCannotResurrectNewerRemove(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour})
	idx.ApplyStore(5, "k", "", 16, 16)
	idx.ApplyRemove(10, "k") // newer removal
	idx.ApplyStore(7, "k", "", 16, 16) // OLD store (seq 7 < 10) must NOT resurrect the removed block
	if tok, _, _ := idx.LookupKey("k"); tok != 0 {
		t.Fatalf("old store must not resurrect newer removal, got %d", tok)
	}
}

func TestIndex_UnresolvedGapZeroesConfidenceAndDirectory(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour})
	idx.ApplyStore(1, "k1", "", 16, 16)
	if c := idx.SnapshotMeta().Confidence; c <= 0 {
		t.Fatalf("precondition: fresh confidence > 0, got %f", c)
	}
	idx.ApplyStore(5, "k2", "", 16, 16) // gap: missed 2,3,4 -> unresolved gap
	s := idx.SnapshotMeta()
	if s.Confidence != 0 {
		t.Fatalf("unresolved gap must zero confidence, got %f", s.Confidence)
	}
	if s.SequenceHealthy || !s.UnresolvedGap {
		t.Fatalf("expected sequence_healthy=false, unresolved_gap=true; got %+v", s)
	}
	if len(idx.Directory(100)) != 0 {
		t.Fatalf("no matchable directory may be published during an unresolved gap")
	}
}

func TestIndex_GapTrustRestoredByReset(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour})
	idx.ApplyStore(1, "k1", "", 16, 16)
	idx.ApplyStore(5, "k2", "", 16, 16) // gap
	if idx.SnapshotMeta().Confidence != 0 {
		t.Fatalf("precondition: gap zeroes confidence")
	}
	idx.Reset("rebuild") // verified rebuild restores trust
	idx.ApplyStore(1, "k3", "", 16, 16)
	s := idx.SnapshotMeta()
	if !s.SequenceHealthy || s.UnresolvedGap {
		t.Fatalf("reset must restore sequence trust, got %+v", s)
	}
	if s.Confidence <= 0 {
		t.Fatalf("confidence must recover after reset+fresh event, got %f", s.Confidence)
	}
}

func TestIndex_GapTrustRestoredByAllClear(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour})
	idx.ApplyStore(1, "k1", "", 16, 16)
	idx.ApplyStore(9, "k2", "", 16, 16) // gap
	if !idx.SnapshotMeta().UnresolvedGap {
		t.Fatalf("precondition: unresolved gap")
	}
	idx.ApplyClear(10) // AllBlocksCleared is a verified rebuild boundary
	if idx.SnapshotMeta().UnresolvedGap {
		t.Fatalf("all-clear must clear the unresolved-gap flag")
	}
}

func TestIndex_StaleCounterIncrementsOncePerTransition(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: 5 * time.Second})
	idx.ApplyStore(1, "k1", "", 16, 16)
	clk.advance(6 * time.Second) // now stale
	// poll many times while stale — counter must increment EXACTLY ONCE (the fresh->stale transition)
	for i := 0; i < 5; i++ {
		_ = idx.SnapshotMeta()
	}
	if got := idx.SnapshotMeta().StaleInvalidations; got != 1 {
		t.Fatalf("stale counter must increment once per fresh->stale transition, got %d", got)
	}
	// refresh -> fresh again, then stale again -> second increment
	idx.ApplyStore(2, "k2", "", 16, 16)
	_ = idx.SnapshotMeta() // observe fresh
	clk.advance(6 * time.Second)
	for i := 0; i < 3; i++ {
		_ = idx.SnapshotMeta()
	}
	if got := idx.SnapshotMeta().StaleInvalidations; got != 2 {
		t.Fatalf("expected 2 stale transitions, got %d", got)
	}
}

func TestIndex_SameSeqDuplicateStoreIdempotent(t *testing.T) {
	clk := newClock(time.Unix(1000, 0))
	idx := newTestIndex(clk, IndexConfig{MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour})
	idx.ApplyStore(3, "k", "", 16, 16)
	idx.ApplyStore(3, "k", "", 16, 16) // exact duplicate (same key+seq)
	s := idx.SnapshotMeta()
	if s.DuplicateEventsTotal != 1 {
		t.Fatalf("expected 1 duplicate, got %d", s.DuplicateEventsTotal)
	}
	if s.IndexEntries != 1 {
		t.Fatalf("expected 1 entry, got %d", s.IndexEntries)
	}
}
