package dataplane

import (
	"sync"
	"sync/atomic"
)

// WorkAccountant tracks OPTIONAL token-level work accounting alongside the hard request-count and
// inflight bounds in Queue (which remain the safety bounds; this is ADVISORY in this task). It is
// correct-by-construction: a reservation is created ONCE at queue admission, moved queued->active at
// dispatch, and released ONCE at the terminal path. The reservation is carried on the request
// ticket, never recomputed.
//
// Conservative reservation rule (NEVER trust cache affinity for safety):
//   READY match (trustworthy)            -> reserve UNCACHED prompt tokens
//   ABSENT / WARMING / unknown / stale   -> reserve FULL prompt tokens
//
// Invariants (enforced + tested): queued>=0, active>=0, outstanding>=0,
// outstanding == queued+active, and all return to 0 after every request terminates.
type WorkAccountant struct {
	mu sync.Mutex

	queuedPrefill int64
	queuedDecode  int64
	activePrefill int64
	activeDecode  int64

	totalReservedPrefill atomic.Int64
	totalReservedDecode  atomic.Int64
}

func NewWorkAccountant() *WorkAccountant { return &WorkAccountant{} }

// Reservation is the per-ticket handle. It records the exact token amounts booked so the SAME
// amounts are moved/released — never recomputed. Use-once: Activate then Release, or Release directly
// (if it never dispatched). Release is idempotent.
type Reservation struct {
	w        *WorkAccountant
	prefill  int
	decode   int
	active   bool
	released bool
	mu       sync.Mutex
}

// Reserve books a queued reservation at ADMISSION time. matchedReadyTokens is the trustworthy READY
// prefix length (0 unless the cache is READY with high confidence); inputTokens/expectedOutput are
// request shape. Returns a Reservation carried on the ticket.
func (w *WorkAccountant) Reserve(inputTokens, matchedReadyTokens, expectedOutput int, readyTrust bool) *Reservation {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if expectedOutput < 0 {
		expectedOutput = 0
	}
	matched := 0
	if readyTrust {
		matched = matchedReadyTokens
		if matched > inputTokens {
			matched = inputTokens
		}
	}
	prefill := inputTokens - matched
	if prefill < 0 {
		prefill = 0
	}
	decode := expectedOutput
	w.mu.Lock()
	w.queuedPrefill += int64(prefill)
	w.queuedDecode += int64(decode)
	w.mu.Unlock()
	w.totalReservedPrefill.Add(int64(prefill))
	w.totalReservedDecode.Add(int64(decode))
	return &Reservation{w: w, prefill: prefill, decode: decode}
}

// Activate moves a reservation queued->active (called at dispatch). Idempotent; no-op after release.
func (r *Reservation) Activate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released || r.active {
		return
	}
	r.active = true
	r.w.mu.Lock()
	r.w.queuedPrefill -= int64(r.prefill)
	r.w.queuedDecode -= int64(r.decode)
	r.w.activePrefill += int64(r.prefill)
	r.w.activeDecode += int64(r.decode)
	r.w.clampLocked()
	r.w.mu.Unlock()
}

// Release frees the reservation from whichever bucket it currently sits in (queued OR active).
// Idempotent: a second Release is a no-op. Called on EVERY terminal path.
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	r.w.mu.Lock()
	if r.active {
		r.w.activePrefill -= int64(r.prefill)
		r.w.activeDecode -= int64(r.decode)
	} else {
		r.w.queuedPrefill -= int64(r.prefill)
		r.w.queuedDecode -= int64(r.decode)
	}
	r.w.clampLocked()
	r.w.mu.Unlock()
}

// clampLocked guards against transient negatives (defense-in-depth; invariants assert >=0).
func (w *WorkAccountant) clampLocked() {
	if w.queuedPrefill < 0 {
		w.queuedPrefill = 0
	}
	if w.queuedDecode < 0 {
		w.queuedDecode = 0
	}
	if w.activePrefill < 0 {
		w.activePrefill = 0
	}
	if w.activeDecode < 0 {
		w.activeDecode = 0
	}
}

// WorkSnapshot is the token-level work-accounting view (exposed via /v1/queue when enabled).
type WorkSnapshot struct {
	QueuedReservedPrefillTokens int64 `json:"queued_reserved_prefill_tokens"`
	QueuedReservedDecodeTokens  int64 `json:"queued_reserved_decode_tokens"`
	ActivePrefillTokens         int64 `json:"active_prefill_tokens"`
	ActiveDecodeTokens          int64 `json:"active_decode_tokens"`
	TotalOutstandingPrefill     int64 `json:"total_outstanding_prefill_tokens"`
	TotalOutstandingDecode      int64 `json:"total_outstanding_decode_tokens"`
	LifetimeReservedPrefill     int64 `json:"lifetime_reserved_prefill_tokens"`
	LifetimeReservedDecode      int64 `json:"lifetime_reserved_decode_tokens"`
}

// Snapshot returns the current counters. outstanding = queued + active (by construction).
func (w *WorkAccountant) Snapshot() WorkSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WorkSnapshot{
		QueuedReservedPrefillTokens: w.queuedPrefill,
		QueuedReservedDecodeTokens:  w.queuedDecode,
		ActivePrefillTokens:         w.activePrefill,
		ActiveDecodeTokens:          w.activeDecode,
		TotalOutstandingPrefill:     w.queuedPrefill + w.activePrefill,
		TotalOutstandingDecode:      w.queuedDecode + w.activeDecode,
		LifetimeReservedPrefill:     w.totalReservedPrefill.Load(),
		LifetimeReservedDecode:      w.totalReservedDecode.Load(),
	}
}
