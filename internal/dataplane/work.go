package dataplane

import (
	"sync"
	"sync/atomic"
)

// WorkAccountant tracks OPTIONAL cache-aware work accounting alongside the hard request-count and
// inflight bounds in Queue. It does NOT gate admission by itself (the queue's MaxQueuedRequests /
// MaxInflightRequests remain the hard safety bounds). It exposes token-level reservations so the
// sidecar and router can reason about prefill/decode pressure beyond raw request counts.
//
// Conservative reservation rule (NEVER trust cache affinity fully for safety):
//   - high cache confidence  -> reserve based on UNCACHED prompt tokens
//   - low / stale confidence -> reserve based on FULL prompt tokens (assume nothing is cached)
//
// All counters are token counts. Reserved* are in-flight-or-queued reservations; Active* are the
// subset currently dispatched (between dispatch and completion). They are released on completion.
type WorkAccountant struct {
	mu sync.Mutex

	reservedUncachedPrefill int64
	reservedDecode          int64
	activeUncachedPrefill   int64
	activeDecode            int64

	// monotonic totals for observability
	totalReservedPrefill atomic.Int64
	totalReservedDecode  atomic.Int64
}

// NewWorkAccountant builds an empty accountant.
func NewWorkAccountant() *WorkAccountant { return &WorkAccountant{} }

// ConfidenceThreshold above which we treat cache locality as trustworthy for reservation sizing.
const WorkConfidenceThreshold = 0.30

// ReserveTokens computes and records a reservation for a request. matchedPrefixTokens/confidence are
// the cache-locality inputs; inputTokens/expectedOutput are request shape. Returns the reserved
// uncached-prefill and decode token counts actually booked (for the trajectory/queue snapshot).
func (w *WorkAccountant) ReserveTokens(inputTokens, matchedPrefixTokens, expectedOutput int, confidence float64) (uncachedPrefill, decode int) {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if expectedOutput < 0 {
		expectedOutput = 0
	}
	// Conservative: only subtract the cached prefix when confidence is high enough.
	matched := 0
	if confidence >= WorkConfidenceThreshold {
		matched = matchedPrefixTokens
		if matched > inputTokens {
			matched = inputTokens
		}
	}
	uncachedPrefill = inputTokens - matched
	if uncachedPrefill < 0 {
		uncachedPrefill = 0
	}
	decode = expectedOutput
	w.mu.Lock()
	w.reservedUncachedPrefill += int64(uncachedPrefill)
	w.reservedDecode += int64(decode)
	w.mu.Unlock()
	w.totalReservedPrefill.Add(int64(uncachedPrefill))
	w.totalReservedDecode.Add(int64(decode))
	return uncachedPrefill, decode
}

// Activate moves a reservation from reserved->active (called at dispatch).
func (w *WorkAccountant) Activate(uncachedPrefill, decode int) {
	w.mu.Lock()
	w.activeUncachedPrefill += int64(uncachedPrefill)
	w.activeDecode += int64(decode)
	w.mu.Unlock()
}

// Release frees a reservation (called at completion/failure/cancel). Clamps at zero.
func (w *WorkAccountant) Release(uncachedPrefill, decode int) {
	w.mu.Lock()
	w.reservedUncachedPrefill -= int64(uncachedPrefill)
	w.reservedDecode -= int64(decode)
	w.activeUncachedPrefill -= int64(uncachedPrefill)
	w.activeDecode -= int64(decode)
	if w.reservedUncachedPrefill < 0 {
		w.reservedUncachedPrefill = 0
	}
	if w.reservedDecode < 0 {
		w.reservedDecode = 0
	}
	if w.activeUncachedPrefill < 0 {
		w.activeUncachedPrefill = 0
	}
	if w.activeDecode < 0 {
		w.activeDecode = 0
	}
	w.mu.Unlock()
}

// WorkSnapshot is the token-level work-accounting view (exposed via /v1/queue when enabled).
type WorkSnapshot struct {
	ReservedUncachedPrefillTokens int64 `json:"reserved_uncached_prefill_tokens"`
	ReservedDecodeTokens          int64 `json:"reserved_decode_tokens"`
	ActiveUncachedPrefillTokens   int64 `json:"active_uncached_prefill_work"`
	ActiveDecodeTokens            int64 `json:"active_decode_work"`
	TotalReservedPrefillTokens    int64 `json:"reserved_prefill_tokens_total"`
	TotalReservedDecodeTokens     int64 `json:"reserved_decode_tokens_total"`
}

// Snapshot returns the current work-accounting counters.
func (w *WorkAccountant) Snapshot() WorkSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WorkSnapshot{
		ReservedUncachedPrefillTokens: w.reservedUncachedPrefill,
		ReservedDecodeTokens:          w.reservedDecode,
		ActiveUncachedPrefillTokens:   w.activeUncachedPrefill,
		ActiveDecodeTokens:            w.activeDecode,
		TotalReservedPrefillTokens:    w.totalReservedPrefill.Load(),
		TotalReservedDecodeTokens:     w.totalReservedDecode.Load(),
	}
}
