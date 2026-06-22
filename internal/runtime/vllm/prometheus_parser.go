// Package vllm implements the runtime.RuntimeAdapter for a vLLM OpenAI-compatible server.
package vllm

import (
	"bufio"
	"strconv"
	"strings"
)

// promSample is one parsed Prometheus metric line: name{labels} value.
type promSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// parsePrometheus parses Prometheus text-exposition format defensively. It never panics;
// malformed lines are skipped. Returns all samples (HELP/TYPE comments ignored).
func parsePrometheus(text string) []promSample {
	var out []promSample
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, ok := parseLine(line)
		if ok {
			out = append(out, s)
		}
	}
	return out
}

func parseLine(line string) (promSample, bool) {
	// forms: name value | name{labels} value [timestamp]
	var s promSample
	name := line
	rest := ""
	if i := strings.IndexAny(line, "{ "); i >= 0 {
		name = line[:i]
		rest = line[i:]
	}
	s.Name = strings.TrimSpace(name)
	if s.Name == "" {
		return s, false
	}
	s.Labels = map[string]string{}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "{") {
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return s, false
		}
		s.Labels = parseLabels(rest[1:end])
		rest = strings.TrimSpace(rest[end+1:])
	}
	// rest now = value [timestamp]
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return s, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, false
	}
	s.Value = v
	return s, true
}

func parseLabels(s string) map[string]string {
	m := map[string]string{}
	// split on commas not inside quotes (labels are key="value")
	var key, val strings.Builder
	inVal := false
	inQuote := false
	flush := func() {
		k := strings.TrimSpace(key.String())
		if k != "" {
			m[k] = val.String()
		}
		key.Reset()
		val.Reset()
		inVal = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == '=' && !inQuote && !inVal:
			inVal = true
		case c == ',' && !inQuote:
			flush()
		default:
			if inVal {
				val.WriteByte(c)
			} else {
				key.WriteByte(c)
			}
		}
	}
	flush()
	return m
}

// sumByName returns the summed value of all samples with the given metric name.
func sumByName(samples []promSample, name string) (float64, bool) {
	var sum float64
	found := false
	for _, s := range samples {
		if s.Name == name {
			sum += s.Value
			found = true
		}
	}
	return sum, found
}

// firstByName returns the first sample value matching name.
func firstByName(samples []promSample, name string) (float64, bool) {
	for _, s := range samples {
		if s.Name == name {
			return s.Value, true
		}
	}
	return 0, false
}

// histogramQuantile estimates a quantile from Prometheus histogram buckets (name+"_bucket").
// Returns the upper bound of the bucket containing the quantile. Approximate but honest.
func histogramQuantile(samples []promSample, name string, q float64) (float64, bool) {
	type bucket struct {
		le    float64
		count float64
	}
	var buckets []bucket
	var total float64
	for _, s := range samples {
		if s.Name != name+"_bucket" {
			continue
		}
		leStr, ok := s.Labels["le"]
		if !ok {
			continue
		}
		var le float64
		if leStr == "+Inf" {
			le = 1e18
		} else {
			f, err := strconv.ParseFloat(leStr, 64)
			if err != nil {
				continue
			}
			le = f
		}
		buckets = append(buckets, bucket{le: le, count: s.Value})
	}
	if len(buckets) == 0 {
		return 0, false
	}
	// buckets are cumulative; the +Inf bucket holds the total
	for _, b := range buckets {
		if b.count > total {
			total = b.count
		}
	}
	if total == 0 {
		return 0, false
	}
	// sort by le
	for i := 0; i < len(buckets); i++ {
		for j := i + 1; j < len(buckets); j++ {
			if buckets[j].le < buckets[i].le {
				buckets[i], buckets[j] = buckets[j], buckets[i]
			}
		}
	}
	target := q * total
	for _, b := range buckets {
		if b.count >= target {
			if b.le >= 1e18 {
				// +Inf bucket: return the previous finite bound if possible
				return buckets[len(buckets)-1].le, true
			}
			return b.le, true
		}
	}
	return buckets[len(buckets)-1].le, true
}
