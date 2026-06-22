// Package runtime defines the normalized local-inference-runtime contract (e.g. vLLM) that the
// sidecar's observation plane publishes alongside GPU telemetry. Runtime metrics are kept on a
// faster refresh loop than slow hardware telemetry and are NEVER scraped synchronously per request.
package runtime

import "time"

// Field carries a value that may be unsupported on a given runtime/version. Mirrors core.Field
// semantics: Supported=false means the metric could not be obtained (never fabricate zeros).
type Field[T any] struct {
	Value     T    `json:"value"`
	Supported bool `json:"supported"`
}

func Sup[T any](v T) Field[T] { return Field[T]{Value: v, Supported: true} }
func Unsup[T any]() Field[T]   { var z T; return Field[T]{Value: z, Supported: false} }

// RuntimeMetrics is the normalized runtime snapshot. Every field is honestly marked supported/unsupported
// based on what the installed vLLM version actually exposes in /metrics.
type RuntimeMetrics struct {
	Timestamp time.Time `json:"timestamp"`
	Healthy   bool      `json:"healthy"`        // /health returned 200 recently
	ScrapeOK  bool      `json:"scrape_ok"`      // last /metrics scrape parsed
	ScrapeAgeMs float64 `json:"scrape_age_ms"`  // freshness of this snapshot
	ScrapeLatencyMs float64 `json:"scrape_latency_ms"`

	// Runtime-level scheduling (DISTINCT from the sidecar admission queue).
	RequestsRunning  Field[float64] `json:"requests_running"`   // vLLM num_requests_running
	RequestsWaiting  Field[float64] `json:"requests_waiting"`   // vLLM num_requests_waiting (runtime queue)
	KVCacheUtil      Field[float64] `json:"kv_cache_utilization"` // [0,1] gpu cache usage
	ActiveSequences  Field[float64] `json:"active_sequences"`

	// Throughput / latency (often histograms or counters).
	PromptThroughput Field[float64] `json:"prompt_tokens_per_s"`
	GenThroughput    Field[float64] `json:"generation_tokens_per_s"`
	RequestLatencyP50 Field[float64] `json:"request_latency_p50_s"`
	QueueLatencyP50  Field[float64] `json:"queue_latency_p50_s"`
	TTFTP50          Field[float64] `json:"ttft_p50_s"`
	TPOTP50          Field[float64] `json:"tpot_p50_s"`

	// Counters (monotonic).
	PreemptionsTotal Field[float64] `json:"preemptions_total"`
	PrefixCacheHits  Field[float64] `json:"prefix_cache_hits_total"`
	PrefixCacheQueries Field[float64] `json:"prefix_cache_queries_total"`
	RequestsSuccess  Field[float64] `json:"requests_success_total"`
	RequestsFailed   Field[float64] `json:"requests_failed_total"`

	// Identity / provenance.
	ModelName      string   `json:"model_name"`
	RuntimeVersion string   `json:"runtime_version"`
	// RawMetricSources maps each normalized field to the raw Prometheus metric name it came from,
	// so a consumer can audit the mapping. Unmapped normalized fields are absent here.
	RawMetricSources map[string]string `json:"raw_metric_sources"`
	UnsupportedFields []string `json:"unsupported_fields"`
}

// RuntimeAdapter is the normalized interface for a local inference runtime.
type RuntimeAdapter interface {
	// Type returns the runtime type id (e.g. "vllm").
	Type() string
	// Health reports whether the runtime is currently serving (cheap, cached).
	Health() bool
	// Snapshot returns the latest materialized runtime metrics (never scrapes synchronously).
	Snapshot() RuntimeMetrics
	// Start begins the background refresh loop.
	Start()
	// Stop halts the refresh loop.
	Stop()
}
