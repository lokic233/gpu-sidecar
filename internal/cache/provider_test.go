package cache

import (
	"context"
	"testing"
	"time"
)

func TestDisabledProvider(t *testing.T) {
	p := NewProvider(ProviderConfig{Mode: ModeDisabled})
	if p.Mode() != ModeDisabled {
		t.Fatalf("expected disabled mode")
	}
	mr := p.Lookup(PrefixQuery{PrefixKeyHash: "abc", PrefixTokens: 100})
	if mr.Supported || mr.MatchedPrefixTokens != 0 || mr.Confidence != 0 {
		t.Fatalf("disabled must report unsupported/zero, got %+v", mr)
	}
	s := p.Snapshot()
	if s.Enabled || s.Supported {
		t.Fatalf("disabled snapshot must be disabled/unsupported")
	}
	if len(p.Directory(100)) != 0 {
		t.Fatalf("disabled directory must be empty")
	}
}

func TestExplicitProvider_ColdThenWarm(t *testing.T) {
	p := NewExplicitProvider(ProviderConfig{Mode: ModeExplicit, Index: IndexConfig{
		MaxEntries: 100, EntryTTL: time.Hour, StaleAfter: time.Hour}})
	_ = p.Start(context.Background())
	key := HashKey("group-hot")
	// cold: never served on this backend yet
	mr := p.Lookup(PrefixQuery{PrefixKeyHash: key, PrefixTokens: 200})
	if mr.MatchedPrefixTokens != 0 {
		t.Fatalf("expected cold (0 matched) on first lookup, got %d", mr.MatchedPrefixTokens)
	}
	if !mr.Supported || !mr.MatchSupported {
		t.Fatalf("explicit mode must be supported+match-capable")
	}
	// observe (this backend served & cached it)
	p.Observe(key, 200)
	// warm: subsequent lookup matches
	mr = p.Lookup(PrefixQuery{PrefixKeyHash: key, PrefixTokens: 200})
	if mr.MatchedPrefixTokens != 200 {
		t.Fatalf("expected warm 200 matched, got %d", mr.MatchedPrefixTokens)
	}
	if mr.Confidence <= 0 {
		t.Fatalf("expected positive confidence when warm")
	}
}

func TestExplicitProvider_NoPrefixKey(t *testing.T) {
	p := NewExplicitProvider(ProviderConfig{Mode: ModeExplicit, Index: DefaultIndexConfig()})
	_ = p.Start(context.Background())
	mr := p.Lookup(PrefixQuery{}) // not eligible
	if mr.MatchedPrefixTokens != 0 {
		t.Fatalf("expected 0 matched for non-eligible")
	}
	if !mr.Supported || mr.Confidence != 1 {
		t.Fatalf("non-eligible request: supported + certain-no-reuse (conf=1), got %+v", mr)
	}
}

func TestExplicitProvider_MatchCappedByClaim(t *testing.T) {
	p := NewExplicitProvider(ProviderConfig{Mode: ModeExplicit, Index: DefaultIndexConfig()})
	_ = p.Start(context.Background())
	key := HashKey("g")
	p.Observe(key, 500) // recorded 500
	mr := p.Lookup(PrefixQuery{PrefixKeyHash: key, PrefixTokens: 100}) // request claims only 100
	if mr.MatchedPrefixTokens != 100 {
		t.Fatalf("expected match capped at claimed 100, got %d", mr.MatchedPrefixTokens)
	}
}

func TestExplicitProvider_RuntimeRestartInvalidates(t *testing.T) {
	p := NewExplicitProvider(ProviderConfig{Mode: ModeExplicit, Index: DefaultIndexConfig()})
	_ = p.Start(context.Background())
	key := HashKey("g")
	p.Observe(key, 200)
	if mr := p.Lookup(PrefixQuery{PrefixKeyHash: key, PrefixTokens: 200}); mr.MatchedPrefixTokens != 200 {
		t.Fatalf("precondition: expected warm")
	}
	p.OnRuntimeRestart()
	if mr := p.Lookup(PrefixQuery{PrefixKeyHash: key, PrefixTokens: 200}); mr.MatchedPrefixTokens != 0 {
		t.Fatalf("expected cold after runtime restart, got %d", mr.MatchedPrefixTokens)
	}
}

func TestExplicitProvider_DirectoryReflectsObservations(t *testing.T) {
	p := NewExplicitProvider(ProviderConfig{Mode: ModeExplicit, Index: DefaultIndexConfig()})
	_ = p.Start(context.Background())
	p.Observe(HashKey("a"), 100)
	p.Observe(HashKey("b"), 200)
	d := p.Directory(100)
	if d[HashKey("a")] != 100 || d[HashKey("b")] != 200 {
		t.Fatalf("directory mismatch: %+v", d)
	}
}

func TestVLLMProvider_HonestBlocker(t *testing.T) {
	p := NewVLLMProvider(ProviderConfig{Mode: ModeVLLMEvents, Index: DefaultIndexConfig()})
	_ = p.Start(context.Background())
	defer p.Stop()
	// ingest some events (metadata only)
	p.Ingest(BlockEvent{Kind: EventBlockStored, Seq: 1, BlockKeyHash: HashKey("b1"), BlockSize: 16})
	p.Ingest(BlockEvent{Kind: EventBlockStored, Seq: 2, BlockKeyHash: HashKey("b2"), BlockSize: 16})
	// snapshot must be supported (observation plane exists) ...
	s := p.Snapshot()
	if !s.Supported {
		t.Fatalf("vllm provider observation plane should be supported")
	}
	// ... but match must be UNSUPPORTED on this stack (the documented blocker), never a fake 0-match.
	if s.MatchSupported {
		t.Fatalf("native per-request match must be unsupported on this stack")
	}
	mr := p.Lookup(PrefixQuery{PrefixKeyHash: HashKey("b1"), PrefixTokens: 16})
	if mr.MatchSupported {
		t.Fatalf("lookup must report match unsupported")
	}
	if mr.MatchedPrefixTokens != 0 || mr.Confidence != 0 {
		t.Fatalf("blocker: must not fabricate a match, got %+v", mr)
	}
	// directory must be empty (router must not get a matchable directory it can't trust)
	if len(p.Directory(100)) != 0 {
		t.Fatalf("vllm directory must be empty on this stack")
	}
}

func TestVLLMProvider_IngestLifecycle(t *testing.T) {
	p := NewVLLMProvider(ProviderConfig{Mode: ModeVLLMEvents, Index: DefaultIndexConfig()})
	_ = p.Start(context.Background())
	defer p.Stop()
	p.Ingest(BlockEvent{Kind: EventBlockStored, Seq: 1, BlockKeyHash: HashKey("b1"), BlockSize: 16})
	p.Ingest(BlockEvent{Kind: EventBlockRemoved, Seq: 2, BlockKeyHash: HashKey("b1")})
	p.Ingest(BlockEvent{Kind: EventAllCleared, Seq: 3})
	s := p.Snapshot()
	if s.ResetsTotal == 0 {
		t.Fatalf("expected a reset from all_cleared")
	}
}

// channelSource is a test EventSource feeding pre-canned events.
type channelSource struct {
	ch chan BlockEvent
}

func (c *channelSource) Events() <-chan BlockEvent { return c.ch }
func (c *channelSource) Start(context.Context) error { return nil }
func (c *channelSource) Stop() error                  { close(c.ch); return nil }

func TestVLLMProvider_SourceIngestLoop(t *testing.T) {
	src := &channelSource{ch: make(chan BlockEvent, 8)}
	p := NewVLLMProvider(ProviderConfig{Mode: ModeVLLMEvents, Index: DefaultIndexConfig()})
	p.SetSource(src)
	_ = p.Start(context.Background())
	for i := 0; i < 5; i++ {
		src.ch <- BlockEvent{Kind: EventBlockStored, Seq: int64(i + 1), BlockKeyHash: HashKey(keyN(i)), BlockSize: 16}
	}
	// give the ingest loop a moment
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Snapshot().IndexEntries >= 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.Snapshot().IndexEntries; got < 5 {
		t.Fatalf("expected >=5 ingested entries, got %d", got)
	}
	_ = p.Stop()
}

func TestHashKey_Stability(t *testing.T) {
	if HashKey("") != "" {
		t.Fatalf("empty input must hash to empty (absence stays absence)")
	}
	a := HashKey("secret-prefix")
	b := HashKey("secret-prefix")
	if a != b {
		t.Fatalf("hash must be stable")
	}
	if a == "secret-prefix" {
		t.Fatalf("hash must not equal the raw key")
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-hex sha256, got len %d", len(a))
	}
}

func TestKVHeadroomPlumbing(t *testing.T) {
	p := NewExplicitProvider(ProviderConfig{Mode: ModeExplicit, Index: DefaultIndexConfig()})
	_ = p.Start(context.Background())
	p.SetKVHeadroom(0.42, true)
	s := p.Snapshot()
	if !s.KVHeadroomSupported || s.KVHeadroom != 0.42 {
		t.Fatalf("expected kv headroom plumbed, got supported=%v hr=%f", s.KVHeadroomSupported, s.KVHeadroom)
	}
}
