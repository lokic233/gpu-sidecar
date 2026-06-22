package dataplane

import (
	"context"
	"sync"
	"testing"
	"time"
)

func noGate() error { return nil }

func newTestQueue(maxQ, maxInflight int) (*Queue, context.CancelFunc) {
	cfg := DefaultQueueConfig()
	cfg.MaxQueuedRequests = maxQ
	cfg.MaxInflightRequests = maxInflight
	cfg.QueueTimeout = 2 * time.Second
	q := NewQueue(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	return q, cancel
}

func TestQueue_FIFOOrdering(t *testing.T) {
	q, cancel := newTestQueue(10, 1)
	defer cancel()
	var order []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	// admit 5; with inflight=1 they dispatch one at a time in FIFO order
	tickets := make([]*Ticket, 5)
	for i := 0; i < 5; i++ {
		tk, err := q.Admit(context.Background(), idOf(i), "r", "b", "h", "0", noGate)
		if err != nil { t.Fatalf("admit %d: %v", i, err) }
		tickets[i] = tk
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(tk *Ticket) {
			defer wg.Done()
			if err := tk.WaitForDispatch(); err != nil { return }
			mu.Lock(); order = append(order, tk.RequestID); mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			q.Done(tk, StateCompleted, "done")
		}(tickets[i])
	}
	wg.Wait()
	for i := 0; i < 5; i++ {
		if order[i] != idOf(i) {
			t.Fatalf("FIFO violated: position %d = %s, want %s (order=%v)", i, order[i], idOf(i), order)
		}
	}
}

func TestQueue_CapacityRejection(t *testing.T) {
	q, cancel := newTestQueue(2, 0) // inflight=0 so nothing dispatches; queue fills
	defer cancel()
	_, e1 := q.Admit(context.Background(), "a", "r", "b", "h", "0", noGate)
	_, e2 := q.Admit(context.Background(), "b", "r", "b", "h", "0", noGate)
	_, e3 := q.Admit(context.Background(), "c", "r", "b", "h", "0", noGate)
	if e1 != nil || e2 != nil { t.Fatalf("first two should admit: %v %v", e1, e2) }
	if e3 != ErrQueueFull { t.Fatalf("third should be ErrQueueFull, got %v", e3) }
	if s := q.Snapshot(); s.Rejected != 1 { t.Fatalf("rejected count should be 1, got %d", s.Rejected) }
}

func TestQueue_Timeout(t *testing.T) {
	cfg := DefaultQueueConfig()
	cfg.MaxInflightRequests = 0 // never dispatch
	cfg.QueueTimeout = 200 * time.Millisecond
	q := NewQueue(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	tk, _ := q.Admit(context.Background(), "a", "r", "b", "h", "0", noGate)
	err := tk.WaitForDispatch()
	if err != ErrQueueTimeout {
		t.Fatalf("want ErrQueueTimeout, got %v", err)
	}
	if tk.State() != StateTimedOut {
		t.Fatalf("want TIMED_OUT state, got %s", tk.State())
	}
}

func TestQueue_CancellationBeforeDispatch(t *testing.T) {
	cfg := DefaultQueueConfig()
	cfg.MaxInflightRequests = 0
	q := NewQueue(cfg, nil)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go q.Run(rctx)
	parent, cancelReq := context.WithCancel(context.Background())
	tk, _ := q.Admit(parent, "a", "r", "b", "h", "0", noGate)
	go func() { time.Sleep(50 * time.Millisecond); cancelReq() }()
	err := tk.WaitForDispatch()
	if err != ErrCancelled {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if tk.State() != StateCancelled {
		t.Fatalf("want CANCELLED, got %s", tk.State())
	}
}

func TestQueue_GateRejection(t *testing.T) {
	q, cancel := newTestQueue(10, 4)
	defer cancel()
	_, err := q.Admit(context.Background(), "a", "r", "b", "h", "0", func() error { return ErrBackendDraining })
	if err != ErrBackendDraining {
		t.Fatalf("gate should reject with ErrBackendDraining, got %v", err)
	}
}

func TestQueue_InflightLimit(t *testing.T) {
	q, cancel := newTestQueue(10, 2)
	defer cancel()
	// admit 4, all wait; only 2 should dispatch (inflight=2), 2 stay queued
	tickets := make([]*Ticket, 4)
	dispatched := make([]bool, 4)
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		tk, _ := q.Admit(context.Background(), idOf(i), "r", "b", "h", "0", noGate)
		tickets[i] = tk
		go func(idx int, t *Ticket) {
			if err := t.WaitForDispatch(); err == nil {
				mu.Lock(); dispatched[idx] = true; mu.Unlock()
			}
		}(i, tk)
	}
	time.Sleep(200 * time.Millisecond)
	s := q.Snapshot()
	if s.Inflight != 2 {
		t.Fatalf("inflight should be 2, got %d", s.Inflight)
	}
	if s.Queued != 2 {
		t.Fatalf("queued should be 2, got %d", s.Queued)
	}
	// complete one inflight; a queued one should promote
	for i := 0; i < 4; i++ {
		mu.Lock(); d := dispatched[i]; mu.Unlock()
		if d { q.Done(tickets[i], StateCompleted, "done"); break }
	}
	time.Sleep(150 * time.Millisecond)
	if s2 := q.Snapshot(); s2.Inflight != 2 || s2.Queued != 1 {
		t.Fatalf("after one done: want inflight=2 queued=1, got inflight=%d queued=%d", s2.Inflight, s2.Queued)
	}
}

func TestQueue_DrainViaClose(t *testing.T) {
	cfg := DefaultQueueConfig()
	cfg.MaxInflightRequests = 0
	q := NewQueue(cfg, nil)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go q.Run(rctx)
	tk, _ := q.Admit(context.Background(), "a", "r", "b", "h", "0", noGate)
	go q.Close()
	if err := tk.WaitForDispatch(); err != ErrCancelled {
		t.Fatalf("close should cancel queued requests, got %v", err)
	}
	// further admits rejected
	if _, err := q.Admit(context.Background(), "b", "r", "b", "h", "0", noGate); err != ErrBackendOffline {
		t.Fatalf("admit after close should be ErrBackendOffline, got %v", err)
	}
}

func TestQueue_Transitions(t *testing.T) {
	var events []TransitionEvent
	var mu sync.Mutex
	cfg := DefaultQueueConfig()
	cfg.MaxInflightRequests = 4
	q := NewQueue(cfg, func(e TransitionEvent) { mu.Lock(); events = append(events, e); mu.Unlock() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	tk, _ := q.Admit(context.Background(), "a", "route1", "h100-gpu3", "h100", "3", noGate)
	tk.WaitForDispatch()
	tk.Transition(StateStreaming, "first_token", q.mono())
	q.Done(tk, StateCompleted, "done")
	time.Sleep(50 * time.Millisecond)
	mu.Lock(); defer mu.Unlock()
	// must have RECEIVED->QUEUED, ...->STREAMING, ...->COMPLETED with full identity
	var sawQueued, sawCompleted bool
	for _, e := range events {
		if e.RequestID != "a" || e.RouteID != "route1" || e.BackendID != "h100-gpu3" {
			t.Fatalf("transition missing identity: %+v", e)
		}
		if e.To == StateQueued { sawQueued = true }
		if e.To == StateCompleted { sawCompleted = true }
	}
	if !sawQueued || !sawCompleted {
		t.Fatalf("missing transitions: queued=%v completed=%v", sawQueued, sawCompleted)
	}
}

func idOf(i int) string { return string(rune('a' + i)) }
