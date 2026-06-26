package vllm

import (
	rt "github.com/lokic233/gpu-sidecar/internal/runtime"
)

// mapVLLMMetrics maps the raw Prometheus exposition into the normalized RuntimeMetrics.
// vLLM metric names are prefixed "vllm:". Names are verified against the installed version at
// startup (see Adapter discovery); unmapped/missing metrics are marked unsupported, never faked.
//
// Known vLLM v1 (>=0.6) gauges/counters/histograms:
//   vllm:num_requests_running (gauge)
//   vllm:num_requests_waiting (gauge)          <- RUNTIME queue, distinct from sidecar queue
//   vllm:gpu_cache_usage_perc (gauge, 0..1)
//   vllm:prompt_tokens_total (counter)
//   vllm:generation_tokens_total (counter)
//   vllm:time_to_first_token_seconds (histogram)
//   vllm:time_per_output_token_seconds (histogram)
//   vllm:e2e_request_latency_seconds (histogram)
//   vllm:request_queue_time_seconds (histogram)
//   vllm:num_preemptions_total (counter)
//   vllm:prefix_cache_hits_total / vllm:prefix_cache_queries_total (counters)
//   vllm:request_success_total (counter)
func mapVLLMMetrics(body string, m *rt.RuntimeMetrics) {
	samples := parsePrometheus(body)
	src := m.RawMetricSources

	setGauge := func(field *rt.Field[float64], names ...string) {
		for _, n := range names {
			if v, ok := firstByName(samples, n); ok {
				*field = rt.Sup(v)
				return
			}
		}
	}
	setSum := func(field *rt.Field[float64], names ...string) {
		for _, n := range names {
			if v, ok := sumByName(samples, n); ok {
				*field = rt.Sup(v)
				return
			}
		}
	}
	setHistP50 := func(field *rt.Field[float64], names ...string) {
		for _, n := range names {
			if v, ok := histogramQuantile(samples, n, 0.5); ok {
				*field = rt.Sup(v)
				src[n] = n + "_bucket"
				return
			}
		}
	}

	*m = rt.RuntimeMetrics{
		Timestamp: m.Timestamp, Healthy: m.Healthy, ScrapeOK: m.ScrapeOK,
		ScrapeLatencyMs: m.ScrapeLatencyMs, ModelName: m.ModelName, RuntimeVersion: m.RuntimeVersion,
		RuntimeInstanceID: m.RuntimeInstanceID,
		RawMetricSources: src, RequestsRunning: rt.Unsup[float64](), RequestsWaiting: rt.Unsup[float64](),
		KVCacheUtil: rt.Unsup[float64](), ActiveSequences: rt.Unsup[float64](),
		PromptThroughput: rt.Unsup[float64](), GenThroughput: rt.Unsup[float64](),
		RequestLatencyP50: rt.Unsup[float64](), QueueLatencyP50: rt.Unsup[float64](),
		TTFTP50: rt.Unsup[float64](), TPOTP50: rt.Unsup[float64](),
		PreemptionsTotal: rt.Unsup[float64](), PrefixCacheHits: rt.Unsup[float64](),
		PrefixCacheQueries: rt.Unsup[float64](), RequestsSuccess: rt.Unsup[float64](),
		RequestsFailed: rt.Unsup[float64](),
	}

	setGauge(&m.RequestsRunning, "vllm:num_requests_running")
	if m.RequestsRunning.Supported { src["requests_running"] = "vllm:num_requests_running" }
	setGauge(&m.RequestsWaiting, "vllm:num_requests_waiting")
	if m.RequestsWaiting.Supported { src["requests_waiting"] = "vllm:num_requests_waiting" }
	setGauge(&m.KVCacheUtil, "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc")
	if m.KVCacheUtil.Supported { src["kv_cache_utilization"] = "vllm:kv_cache_usage_perc" }
	// active sequences ~ running requests when no dedicated metric
	if m.RequestsRunning.Supported {
		m.ActiveSequences = m.RequestsRunning
		src["active_sequences"] = "derived:vllm:num_requests_running"
	}
	setSum(&m.PromptThroughput, "vllm:prompt_tokens_total")
	if m.PromptThroughput.Supported { src["prompt_tokens_per_s"] = "vllm:prompt_tokens_total(counter)" }
	setSum(&m.GenThroughput, "vllm:generation_tokens_total")
	if m.GenThroughput.Supported { src["generation_tokens_per_s"] = "vllm:generation_tokens_total(counter)" }
	setHistP50(&m.TTFTP50, "vllm:time_to_first_token_seconds")
	setHistP50(&m.TPOTP50, "vllm:inter_token_latency_seconds", "vllm:request_time_per_output_token_seconds", "vllm:time_per_output_token_seconds")
	setHistP50(&m.RequestLatencyP50, "vllm:e2e_request_latency_seconds")
	setHistP50(&m.QueueLatencyP50, "vllm:request_queue_time_seconds")
	setSum(&m.PreemptionsTotal, "vllm:num_preemptions_total")
	if m.PreemptionsTotal.Supported { src["preemptions_total"] = "vllm:num_preemptions_total" }
	setSum(&m.PrefixCacheHits, "vllm:prefix_cache_hits_total")
	setSum(&m.PrefixCacheQueries, "vllm:prefix_cache_queries_total")
	setSum(&m.RequestsSuccess, "vllm:request_success_total")
	setSum(&m.RequestsFailed, "vllm:request_failure_total")
	// Runtime instance identity: process_start_time_seconds uniquely fingerprints the upstream PROCESS.
	if v, ok := firstByName(samples, "process_start_time_seconds"); ok {
		m.RuntimeInstanceID = v
		src["runtime_instance_id"] = "process_start_time_seconds"
	}

	// Honestly record which normalized fields are unsupported on this version.
	type fc struct { name string; f rt.Field[float64] }
	for _, p := range []fc{
		{"requests_running", m.RequestsRunning}, {"requests_waiting", m.RequestsWaiting},
		{"kv_cache_utilization", m.KVCacheUtil}, {"prompt_tokens_per_s", m.PromptThroughput},
		{"generation_tokens_per_s", m.GenThroughput}, {"ttft_p50_s", m.TTFTP50},
		{"tpot_p50_s", m.TPOTP50}, {"request_latency_p50_s", m.RequestLatencyP50},
		{"queue_latency_p50_s", m.QueueLatencyP50}, {"preemptions_total", m.PreemptionsTotal},
		{"prefix_cache_hits_total", m.PrefixCacheHits}, {"requests_success_total", m.RequestsSuccess},
		{"requests_failed_total", m.RequestsFailed},
	} {
		if !p.f.Supported {
			m.UnsupportedFields = append(m.UnsupportedFields, p.name)
		}
	}
}
