package vllm
import rt "github.com/lokic233/gpu-sidecar/internal/runtime"
// ParseForTest exposes mapVLLMMetrics for validation/testing.
func ParseForTest(body string) rt.RuntimeMetrics {
	m := rt.RuntimeMetrics{RawMetricSources: map[string]string{}}
	mapVLLMMetrics(body, &m)
	return m
}
