package cache

import (
	"context"
	"sync"
	"sync/atomic"
)

// EventKind enumerates the native KV block-lifecycle event types (mirrors vLLM's
// distributed/kv_events.py: BlockStored / BlockRemoved / AllBlocksCleared).
type EventKind string

const (
	EventBlockStored  EventKind = "block_stored"
	EventBlockRemoved EventKind = "block_removed"
	EventAllCleared   EventKind = "all_blocks_cleared"
)

// BlockEvent is the SANITIZED, metadata-only form of a native vLLM KV event. The transport adapter
// MUST convert the raw vLLM event into this form BEFORE it reaches the provider:
//   - vLLM's BlockStored carries raw token_ids — these are DROPPED here (never enter Go).
//   - block hashes are HASHED again into opaque keys (BlockKeyHash) so no raw runtime hash is stored.
// Seq is the publisher's monotone sequence number (from the ZMQ multipart frame).
type BlockEvent struct {
	Kind          EventKind
	Seq           int64
	BlockKeyHash  string // opaque, hashed block key ("" for all_cleared)
	ParentKeyHash string // opaque, hashed parent block key ("" if none)
	BlockSize     int    // tokens per block (BlockStored only)
}

// EventSource is the pluggable transport that delivers sanitized BlockEvents. The native ZMQ
// transport is intentionally NOT compiled in (libzmq + msgpack + verified block-hash matching are
// the documented native-integration blocker on this stack). Tests and the experimental harness use
// a channel-backed source; a future ZMQ adapter implements this same interface.
type EventSource interface {
	// Events returns a receive-only channel of sanitized events. Closed when the source stops.
	Events() <-chan BlockEvent
	// Start begins delivery. Stop halts it.
	Start(ctx context.Context) error
	Stop() error
}

// VLLMProvider ingests native KV block-lifecycle events into the bounded prefix index. It is
// METADATA-ONLY and, on this stack, reports MatchSupported=false: request-side prefix hashes cannot
// be reliably matched to runtime block hashes here (see audit + vllm_match.go). It still provides a
// truthful aggregate cache snapshot (index size, confidence, sequence health, KV headroom) and can
// be upgraded to per-request matching once a verified matcher exists.
type VLLMProvider struct {
	cfg    ProviderConfig
	index  *Index
	source EventSource

	mu          sync.Mutex
	kvHeadroom  float64
	kvSupported bool
	started     atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// matchSupported is false on this stack. A future verified matcher flips it on.
	matchSupported bool
}

// NewVLLMProvider builds a native-events provider. The EventSource is attached separately via
// SetSource (so the binary can wire a ZMQ adapter without this package depending on libzmq).
func NewVLLMProvider(cfg ProviderConfig) *VLLMProvider {
	if cfg.Index.MaxEntries == 0 {
		cfg.Index = DefaultIndexConfig()
	}
	return &VLLMProvider{cfg: cfg, index: NewIndex(cfg.Index), matchSupported: false}
}

// SetSource attaches the event transport. Must be called before Start.
func (p *VLLMProvider) SetSource(s EventSource) { p.source = s }

// IndexForTest exposes the underlying index (tests only).
func (p *VLLMProvider) IndexForTest() *Index { return p.index }

func (p *VLLMProvider) Mode() Mode { return ModeVLLMEvents }

func (p *VLLMProvider) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.started.Store(true)
	if p.source == nil {
		// No transport wired (the default on this stack). The provider is "supported" as an
		// observation plane but will have no events and therefore confidence 0 / not ready.
		return nil
	}
	if err := p.source.Start(p.ctx); err != nil {
		return err
	}
	p.wg.Add(1)
	go p.ingestLoop()
	return nil
}

func (p *VLLMProvider) ingestLoop() {
	defer p.wg.Done()
	ch := p.source.Events()
	for {
		select {
		case <-p.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			p.Ingest(ev)
		}
	}
}

// Ingest applies one sanitized BlockEvent to the index. Exported so tests/harness can feed events
// without a transport. NEVER receives raw token ids (sanitized upstream).
func (p *VLLMProvider) Ingest(ev BlockEvent) {
	switch ev.Kind {
	case EventBlockStored:
		bs := ev.BlockSize
		p.index.ApplyStore(ev.Seq, ev.BlockKeyHash, ev.ParentKeyHash, bs, bs)
	case EventBlockRemoved:
		p.index.ApplyRemove(ev.Seq, ev.BlockKeyHash)
	case EventAllCleared:
		p.index.ApplyClear(ev.Seq)
	}
}

func (p *VLLMProvider) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.source != nil {
		_ = p.source.Stop()
	}
	p.wg.Wait()
	return nil
}

// OnRuntimeRestart invalidates locality on observed runtime restart.
func (p *VLLMProvider) OnRuntimeRestart() { p.index.Reset("runtime_restart") }

func (p *VLLMProvider) SetKVHeadroom(h float64, supported bool) {
	p.mu.Lock()
	p.kvHeadroom = h
	p.kvSupported = supported
	p.mu.Unlock()
}

// Lookup is HONEST about the blocker: per-request prefix matching against native block hashes is not
// verifiable on this stack, so we never fabricate a match from aggregate state. We report
// Supported=true (the observation plane exists) but MatchSupported=false and 0 matched tokens, which
// makes the cache-aware policy fall back to the load-only estimate for THIS request — exactly the
// required safe behavior. (A future verified matcher would set matchSupported=true and answer here.)
func (p *VLLMProvider) Lookup(q PrefixQuery) MatchResult {
	if !p.matchSupported {
		return MatchResult{Supported: true, MatchSupported: false, MatchedPrefixTokens: 0,
			Confidence: 0, Reason: "native_match_unsupported_on_stack"}
	}
	// (unreachable on this stack) verified-matcher path:
	recorded, conf, ageMs := p.index.LookupKey(q.PrefixKeyHash)
	if recorded == 0 || conf <= 0 {
		return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: 0,
			Confidence: conf, SnapshotAgeMs: ageMs, Reason: "cold_or_stale"}
	}
	matched := q.PrefixTokens
	if recorded < matched {
		matched = recorded
	}
	return MatchResult{Supported: true, MatchSupported: true, MatchedPrefixTokens: matched,
		Confidence: conf, SnapshotAgeMs: ageMs, Reason: "warm_block_match"}
}

// Directory is EMPTY on this stack: native request->block-hash matching is not verifiable, so the
// router must not be handed a per-request matchable directory. (A future verified matcher returns
// p.index.Directory(max) here.)
func (p *VLLMProvider) Directory(max int) map[string]int {
	if !p.matchSupported {
		return map[string]int{}
	}
	return p.index.Directory(max)
}

func (p *VLLMProvider) Snapshot() Snapshot {
	s := p.index.SnapshotMeta()
	s.Enabled = true
	s.Provider = string(ModeVLLMEvents)
	s.Supported = true
	s.MatchSupported = p.matchSupported
	p.mu.Lock()
	s.KVHeadroom = p.kvHeadroom
	s.KVHeadroomSupported = p.kvSupported
	p.mu.Unlock()
	return s
}
