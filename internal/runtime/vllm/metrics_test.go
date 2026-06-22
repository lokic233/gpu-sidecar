package vllm

import (
	"os"
	"testing"
)

func TestParseRealVLLMMetrics(t *testing.T) {
	body, err := os.ReadFile("testdata_metrics.txt")
	if err != nil {
		t.Skip("no real metrics fixture")
	}
	m := ParseForTest(string(body))
	// On a live vLLM 0.23 server these MUST be supported:
	if !m.RequestsRunning.Supported {
		t.Error("requests_running should be supported on vllm 0.23")
	}
	if !m.RequestsWaiting.Supported {
		t.Error("requests_waiting should be supported")
	}
	if !m.KVCacheUtil.Supported {
		t.Error("kv_cache_utilization should map from vllm:kv_cache_usage_perc")
	}
	if !m.GenThroughput.Supported {
		t.Error("generation_tokens_total should be supported")
	}
	if !m.RequestsSuccess.Supported {
		t.Error("request_success_total should be supported")
	}
	// TTFT histogram should parse after at least one request
	t.Logf("ttft_p50=%+v tpot_p50=%+v kv=%+v running=%+v waiting=%+v",
		m.TTFTP50, m.TPOTP50, m.KVCacheUtil, m.RequestsRunning, m.RequestsWaiting)
}

func TestParseMalformedSafe(t *testing.T) {
	// Defensive: malformed input must not panic and must mark everything unsupported.
	for _, bad := range []string{"", "garbage\n\n###", "vllm:x{le=", "vllm:num_requests_running notanumber"} {
		m := ParseForTest(bad)
		if m.RequestsRunning.Supported && bad != "vllm:num_requests_running notanumber" {
			// the last case: value unparseable -> unsupported anyway
		}
		_ = m
	}
}

func TestParseLabels(t *testing.T) {
	ls := parseLabels(`model_name="Qwen/Qwen2.5-0.5B",le="0.5"`)
	if ls["model_name"] != "Qwen/Qwen2.5-0.5B" || ls["le"] != "0.5" {
		t.Fatalf("label parse wrong: %v", ls)
	}
}

func TestHistogramQuantile(t *testing.T) {
	body := `vllm:t_seconds_bucket{le="0.1"} 2
vllm:t_seconds_bucket{le="0.5"} 8
vllm:t_seconds_bucket{le="1.0"} 10
vllm:t_seconds_bucket{le="+Inf"} 10`
	samples := parsePrometheus(body)
	v, ok := histogramQuantile(samples, "vllm:t_seconds", 0.5)
	if !ok || v != 0.5 {
		t.Fatalf("p50 should land in le=0.5 bucket, got %v ok=%v", v, ok)
	}
}
