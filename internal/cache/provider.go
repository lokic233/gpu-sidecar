package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Provider is the pluggable cache-observation contract. Implementations are: disabled (no-op),
// explicit (deterministic experiment keys), and vllm_events (native, metadata-only).
//
// Lifecycle: Start begins any background ingest loop; Stop halts it. Snapshot returns bounded
// metadata (hot-path safe, no blocking I/O). Lookup answers a per-request prefix-locality query
// from already-ingested local state (no synchronous runtime scrape).
type Provider interface {
	// Mode identifies the provider ("disabled" | "explicit" | "vllm_events").
	Mode() Mode
	// Start begins background ingestion (if any). Must be non-blocking beyond setup.
	Start(ctx context.Context) error
	// Stop halts background ingestion and releases resources. Idempotent.
	Stop() error
	// Snapshot returns bounded cache-observation metadata.
	Snapshot() Snapshot
	// Lookup answers a per-request prefix-locality query for THIS backend from local state.
	Lookup(q PrefixQuery) MatchResult
	// OnRuntimeRestart should be called when the local runtime is observed to have restarted, so the
	// provider invalidates locality (confidence -> 0 until re-warmed).
	OnRuntimeRestart()
	// SetKVHeadroom feeds the latest KV headroom (1 - kv_cache_util) from the runtime adapter so the
	// /v1/cache snapshot can expose it. supported=false when the runtime does not report KV usage.
	SetKVHeadroom(headroom float64, supported bool)
	// Directory returns a bounded map of opaque (hashed) prefix keys -> matched token counts for the
	// router to materialize off the hot path. Empty when disabled / not match-capable / stale.
	Directory(max int) map[string]int
}

// HashKey returns a hex-encoded SHA-256 of an opaque key. Used to ensure raw experiment keys are
// NEVER stored or emitted. Stable across processes (so the router and sidecar agree). An empty input
// hashes to "" (not eligible) rather than the hash of the empty string, so absence stays absence.
func HashKey(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ProviderConfig is the union of provider configuration knobs (set from flags).
type ProviderConfig struct {
	Mode  Mode
	Index IndexConfig

	// explicit mode
	ExplicitHeaderEnabled bool

	// vllm_events mode
	EventEndpoint string // transport endpoint (e.g. tcp://127.0.0.1:5557); empty => not wired
}

// NewProvider builds a provider for the configured mode. Unknown/disabled => DisabledProvider.
func NewProvider(cfg ProviderConfig) Provider {
	switch cfg.Mode {
	case ModeExplicit:
		return NewExplicitProvider(cfg)
	case ModeVLLMEvents:
		return NewVLLMProvider(cfg)
	default:
		return NewDisabledProvider()
	}
}

// --- disabled provider -------------------------------------------------------------------------

// DisabledProvider is the default: reports unsupported, matches nothing. Cache-aware routing then
// falls back to the existing load-only estimate.
type DisabledProvider struct{}

func NewDisabledProvider() *DisabledProvider { return &DisabledProvider{} }

func (p *DisabledProvider) Mode() Mode                    { return ModeDisabled }
func (p *DisabledProvider) Start(context.Context) error   { return nil }
func (p *DisabledProvider) Stop() error                   { return nil }
func (p *DisabledProvider) OnRuntimeRestart()             {}
func (p *DisabledProvider) SetKVHeadroom(float64, bool)   {}
func (p *DisabledProvider) Lookup(PrefixQuery) MatchResult {
	return NoMatch("cache_observation_disabled")
}
func (p *DisabledProvider) Directory(int) map[string]int { return map[string]int{} }
func (p *DisabledProvider) Snapshot() Snapshot {
	return Snapshot{Enabled: false, Provider: string(ModeDisabled), Supported: false,
		MatchSupported: false, Ready: false, Confidence: 0, SnapshotAgeMs: -1, LastEventSequence: -1}
}
