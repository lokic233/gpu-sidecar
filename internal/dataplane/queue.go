// Package dataplane implements the sidecar local data plane: a bounded host-level admission queue
// in front of the local vLLM server, request lifecycle ownership, and dispatch. This is DISTINCT
// from vLLM's internal runtime scheduling queue (exposed separately in runtime metrics).
package dataplane

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ReqState is the local request lifecycle state.
type ReqState string

const (
	StateReceived    ReqState = "RECEIVED"
	StateQueued      ReqState = "QUEUED"
	StateDispatching ReqState = "DISPATCHING"
	StateStreaming   ReqState = "STREAMING"
	StateWaitingFull ReqState = "WAITING_FOR_FULL_RESPONSE"
	StateCompleted   ReqState = "COMPLETED"
	// failure states
	StateRejected      ReqState = "REJECTED"
	StateCancelled     ReqState = "CANCELLED"
	StateTimedOut      ReqState = "TIMED_OUT"
	StateUpstreamFail  ReqState = "UPSTREAM_FAILED"
	StatePartialStream ReqState = "PARTIAL_STREAM_FAILED"
)

// Rejection reasons (structured errors returned to the router).
var (
	ErrQueueFull        = errors.New("ADMISSION_QUEUE_FULL")
	ErrBackendOffline   = errors.New("BACKEND_OFFLINE")
	ErrBackendDraining  = errors.New("BACKEND_DRAINING")
	ErrRuntimeUnhealthy = errors.New("RUNTIME_UNHEALTHY")
	ErrQueueTimeout     = errors.New("QUEUE_TIMEOUT")
	ErrInflightFull     = errors.New("INFLIGHT_LIMIT")
)

// QueueConfig configures the bounded admission queue.
type QueueConfig struct {
	Enabled            bool
	MaxQueuedRequests  int
	MaxInflightRequests int
	QueueTimeout       time.Duration
	AdmissionMode      string // "fifo" (default)
}

func DefaultQueueConfig() QueueConfig {
	return QueueConfig{
		Enabled: true, MaxQueuedRequests: 256, MaxInflightRequests: 32,
		QueueTimeout: 30 * time.Second, AdmissionMode: "fifo",
	}
}

// Ticket is a handle for an admitted request. The data plane dispatches it; the caller waits on
// Dispatched() and, when done, calls Done()/Fail().
type Ticket struct {
	RequestID string
	RouteID   string
	BackendID string
	HostID    string
	DeviceID  string

	enqueuedMono time.Duration
	enqueuedWall time.Time
	dispatchMono time.Duration

	dispatch chan struct{} // closed when admitted-to-dispatch
	cancel   context.CancelFunc
	ctx      context.Context

	q *Queue
	state ReqState
	mu    sync.Mutex

	// --- cache/work lifecycle carried on the ticket (Round-5 hardening) ---
	// reservation is the token work-accounting handle (nil when work accounting is off). Created at
	// admission, activated at dispatch, released ONCE on the terminal path.
	reservation *Reservation
	// prefixKeyHash is the HASHED explicit-prefix key for this request ("" when not cache-eligible).
	prefixKeyHash string
	// prefixTokens is the claimed prefix length (bounded).
	prefixTokens int
	// warmBegun is true once BeginWarm was called, so the terminal path knows to MarkReady/AbortWarm.
	warmBegun bool
	// resolved guards the cache/work terminal resolution so it runs exactly once.
	resolved bool
}

func (t *Ticket) Context() context.Context { return t.ctx }
func (t *Ticket) State() ReqState { t.mu.Lock(); defer t.mu.Unlock(); return t.state }

// Transition emits a state transition via the queue's transition hook.
func (t *Ticket) Transition(to ReqState, reason string, mono time.Duration) {
	t.mu.Lock()
	from := t.state
	t.state = to
	t.mu.Unlock()
	if t.q != nil && t.q.onTransition != nil {
		t.q.onTransition(TransitionEvent{
			RequestID: t.RequestID, RouteID: t.RouteID, BackendID: t.BackendID,
			HostID: t.HostID, DeviceID: t.DeviceID, Wall: time.Now(), Mono: mono,
			From: from, To: to, Reason: reason,
		})
	}
}

// TransitionEvent is emitted on every request state transition.
type TransitionEvent struct {
	RequestID, RouteID, BackendID, HostID, DeviceID string
	Wall                                            time.Time
	Mono                                            time.Duration
	From, To                                        ReqState
	Reason                                          string
}

// Queue is the bounded FIFO admission queue + in-flight limiter.
type Queue struct {
	cfg QueueConfig
	mu  sync.Mutex

	waiting   []*Ticket // FIFO queued (not yet dispatched)
	inflight  int
	closed    bool

	// metrics counters (monotonic)
	arrivals    uint64
	dispatched  uint64
	completed   uint64
	rejected    uint64
	timedOut    uint64
	cancelled   uint64

	// rate windows
	rateWindow time.Duration
	arrivalTimes  []time.Duration
	dispatchTimes []time.Duration
	completeTimes []time.Duration
	waitSamples   []float64 // queue wait durations (ms) for distribution

	onTransition func(TransitionEvent)

	monoStart time.Time
	signal    chan struct{} // wakes the dispatcher
}

func NewQueue(cfg QueueConfig, onTransition func(TransitionEvent)) *Queue {
	return &Queue{
		cfg: cfg, onTransition: onTransition, rateWindow: 10 * time.Second,
		monoStart: time.Now(), signal: make(chan struct{}, 1),
	}
}

func (q *Queue) mono() time.Duration { return time.Since(q.monoStart) }

// Admit attempts to enqueue a request. Returns a Ticket on success, or a structured error.
// gateOK reports whether the backend currently accepts admissions (lifecycle/health/drain checks).
func (q *Queue) Admit(parent context.Context, reqID, routeID, backendID, hostID, deviceID string,
	gate AdmissionGate) (*Ticket, error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil, ErrBackendOffline
	}
	// admission gate checks (lifecycle/health/drain)
	if reason := gate(); reason != nil {
		q.rejected++
		q.mu.Unlock()
		return nil, reason
	}
	if len(q.waiting) >= q.cfg.MaxQueuedRequests {
		q.rejected++
		q.mu.Unlock()
		return nil, ErrQueueFull
	}
	now := q.mono()
	q.arrivals++
	q.arrivalTimes = append(q.arrivalTimes, now)
	ctx, cancel := context.WithCancel(parent)
	t := &Ticket{
		RequestID: reqID, RouteID: routeID, BackendID: backendID, HostID: hostID, DeviceID: deviceID,
		enqueuedMono: now, enqueuedWall: time.Now(), dispatch: make(chan struct{}),
		cancel: cancel, ctx: ctx, q: q, state: StateReceived,
	}
	q.waiting = append(q.waiting, t)
	q.mu.Unlock()
	t.Transition(StateQueued, "enqueued", now)
	q.wake()
	return t, nil
}

// AdmissionGate returns a non-nil error if admission must be refused right now.
type AdmissionGate func() error

func (q *Queue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// WaitForDispatch blocks until the ticket is dispatched, the queue timeout fires, or the request
// is cancelled. Returns nil when dispatched (caller should proceed to upstream), else an error.
func (t *Ticket) WaitForDispatch() error {
	q := t.q
	var timeout <-chan time.Time
	if q.cfg.QueueTimeout > 0 {
		tm := time.NewTimer(q.cfg.QueueTimeout)
		defer tm.Stop()
		timeout = tm.C
	}
	select {
	case <-t.dispatch:
		return nil
	case <-t.ctx.Done():
		q.removeWaiting(t)
		q.mu.Lock(); q.cancelled++; q.mu.Unlock()
		t.Transition(StateCancelled, "client_cancelled_in_queue", q.mono())
		return ErrCancelled
	case <-timeout:
		q.removeWaiting(t)
		q.mu.Lock(); q.timedOut++; q.mu.Unlock()
		t.Transition(StateTimedOut, "queue_timeout", q.mono())
		return ErrQueueTimeout
	}
}

var ErrCancelled = errors.New("CANCELLED")

func (q *Queue) removeWaiting(t *Ticket) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, w := range q.waiting {
		if w == t {
			q.waiting = append(q.waiting[:i], q.waiting[i+1:]...)
			return
		}
	}
}

// Run is the dispatcher loop: promotes queued tickets to dispatch when an in-flight slot is free.
func (q *Queue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.signal:
			q.drain()
		case <-time.After(50 * time.Millisecond):
			q.drain() // periodic safety tick (handles inflight slot frees)
		}
	}
}

func (q *Queue) drain() {
	for {
		q.mu.Lock()
		if len(q.waiting) == 0 || q.inflight >= q.cfg.MaxInflightRequests {
			q.mu.Unlock()
			return
		}
		t := q.waiting[0]
		q.waiting = q.waiting[1:]
		q.inflight++
		q.dispatched++
		now := q.mono()
		q.dispatchTimes = append(q.dispatchTimes, now)
		waitMs := float64((now - t.enqueuedMono).Microseconds()) / 1000.0
		q.waitSamples = append(q.waitSamples, waitMs)
		if len(q.waitSamples) > 4096 {
			q.waitSamples = q.waitSamples[len(q.waitSamples)-4096:]
		}
		q.mu.Unlock()
		t.dispatchMono = now
		close(t.dispatch) // unblocks WaitForDispatch
	}
}

// Done marks a dispatched request complete and frees its in-flight slot.
func (q *Queue) Done(t *Ticket, final ReqState, reason string) {
	q.mu.Lock()
	if q.inflight > 0 {
		q.inflight--
	}
	q.completed++
	q.completeTimes = append(q.completeTimes, q.mono())
	q.mu.Unlock()
	t.Transition(final, reason, q.mono())
	q.wake()
}

// Close drains and rejects (used on shutdown).
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	w := q.waiting
	q.waiting = nil
	q.mu.Unlock()
	for _, t := range w {
		t.cancel()
	}
}

// Snapshot returns the queue metrics (host-level admission queue, NOT vLLM's runtime queue).
type Snapshot struct {
	Enabled        bool    `json:"enabled"`
	Queued         int     `json:"queued_requests"`
	Inflight       int     `json:"inflight_requests"`
	MaxQueued      int     `json:"max_queued_requests"`
	MaxInflight    int     `json:"max_inflight_requests"`
	OldestAgeMs    float64 `json:"oldest_queued_age_ms"`
	ArrivalRate    float64 `json:"arrival_rate_per_s"`
	DispatchRate   float64 `json:"dispatch_rate_per_s"`
	CompletionRate float64 `json:"completion_rate_per_s"`
	Arrivals       uint64  `json:"arrivals_total"`
	Dispatched     uint64  `json:"dispatched_total"`
	Completed      uint64  `json:"completed_total"`
	Rejected       uint64  `json:"rejected_total"`
	TimedOut       uint64  `json:"queue_timeout_total"`
	Cancelled      uint64  `json:"cancelled_total"`
	WaitP50Ms      float64 `json:"queue_wait_p50_ms"`
	WaitP95Ms      float64 `json:"queue_wait_p95_ms"`
}

func (q *Queue) Snapshot() Snapshot {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.mono()
	s := Snapshot{
		Enabled: q.cfg.Enabled, Queued: len(q.waiting), Inflight: q.inflight,
		MaxQueued: q.cfg.MaxQueuedRequests, MaxInflight: q.cfg.MaxInflightRequests,
		Arrivals: q.arrivals, Dispatched: q.dispatched, Completed: q.completed,
		Rejected: q.rejected, TimedOut: q.timedOut, Cancelled: q.cancelled,
	}
	if len(q.waiting) > 0 {
		s.OldestAgeMs = float64((now - q.waiting[0].enqueuedMono).Microseconds()) / 1000.0
	}
	s.ArrivalRate = rateInWindow(q.arrivalTimes, now, q.rateWindow)
	s.DispatchRate = rateInWindow(q.dispatchTimes, now, q.rateWindow)
	s.CompletionRate = rateInWindow(q.completeTimes, now, q.rateWindow)
	s.WaitP50Ms = pctile(q.waitSamples, 50)
	s.WaitP95Ms = pctile(q.waitSamples, 95)
	// prune old rate timestamps to keep bounded
	q.arrivalTimes = pruneOld(q.arrivalTimes, now, q.rateWindow)
	q.dispatchTimes = pruneOld(q.dispatchTimes, now, q.rateWindow)
	q.completeTimes = pruneOld(q.completeTimes, now, q.rateWindow)
	return s
}

func rateInWindow(times []time.Duration, now, window time.Duration) float64 {
	cutoff := now - window
	c := 0
	for _, t := range times {
		if t >= cutoff {
			c++
		}
	}
	return float64(c) / window.Seconds()
}

func pruneOld(times []time.Duration, now, window time.Duration) []time.Duration {
	cutoff := now - window
	i := 0
	for i < len(times) && times[i] < cutoff {
		i++
	}
	return times[i:]
}

func pctile(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	cp := append([]float64(nil), s...)
	sort.Float64s(cp)
	if len(cp) == 1 {
		return cp[0]
	}
	rank := (p / 100.0) * float64(len(cp)-1)
	lo := int(rank)
	if lo >= len(cp)-1 {
		return cp[len(cp)-1]
	}
	frac := rank - float64(lo)
	return cp[lo] + frac*(cp[lo+1]-cp[lo])
}
