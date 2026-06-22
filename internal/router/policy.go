package router

import (
	"errors"
	"sync/atomic"
)

// RequestFeatures are the request-side observation inputs available to a routing policy.
// (This is the stable seam Liangqi's future RL policy plugs into.)
type RequestFeatures struct {
	RequestID       string
	Model           string
	Stream          bool
	InputLenEst     int
	RequestedOutput int
	SLOClass        string
}

// RouteDecision is a policy's output.
type RouteDecision struct {
	BackendID  string
	PolicyName string
	PolicyVersion string
	Reason     string
}

// RoutingPolicy selects a backend from an already-materialized snapshot. Implementations MUST be
// pure and fast (no I/O): the snapshot is read-only and pre-fetched off the hot path.
type RoutingPolicy interface {
	SelectBackend(request RequestFeatures, snapshot *BackendSnapshot) (RouteDecision, error)
}

var ErrNoEligibleBackend = errors.New("NO_ELIGIBLE_BACKEND")

// eligible returns backends that can currently accept traffic (reachable, ready, not OFFLINE/DRAINING,
// runtime healthy, queue not full).
func eligible(snap *BackendSnapshot) []BackendState {
	var out []BackendState
	for _, b := range snap.Backends {
		if !b.Reachable || !b.RuntimeHealthy {
			continue
		}
		if b.LifecycleState == "OFFLINE" || b.LifecycleState == "DRAINING" {
			continue
		}
		if b.QueueMax > 0 && b.QueueDepth >= b.QueueMax {
			continue
		}
		out = append(out, b)
	}
	return out
}

// RoundRobinPolicy cycles through eligible backends deterministically.
type RoundRobinPolicy struct{ n atomic.Uint64 }

func (p *RoundRobinPolicy) SelectBackend(_ RequestFeatures, snap *BackendSnapshot) (RouteDecision, error) {
	e := eligible(snap)
	if len(e) == 0 {
		return RouteDecision{}, ErrNoEligibleBackend
	}
	i := p.n.Add(1) - 1
	b := e[int(i)%len(e)]
	return RouteDecision{BackendID: b.Backend.ID, PolicyName: "round_robin", PolicyVersion: "1", Reason: "rr"}, nil
}

// LeastQueuedPolicy picks the eligible backend with the smallest host admission queue depth+inflight.
type LeastQueuedPolicy struct{}

func (p *LeastQueuedPolicy) SelectBackend(_ RequestFeatures, snap *BackendSnapshot) (RouteDecision, error) {
	e := eligible(snap)
	if len(e) == 0 {
		return RouteDecision{}, ErrNoEligibleBackend
	}
	best := e[0]
	for _, b := range e[1:] {
		if (b.QueueDepth+b.QueueInflight) < (best.QueueDepth+best.QueueInflight) {
			best = b
		}
	}
	return RouteDecision{BackendID: best.Backend.ID, PolicyName: "least_queued", PolicyVersion: "1",
		Reason: "min(queue_depth+inflight)"}, nil
}

// LeastRuntimeWaitingPolicy picks the eligible backend with the fewest vLLM waiting requests.
type LeastRuntimeWaitingPolicy struct{}

func (p *LeastRuntimeWaitingPolicy) SelectBackend(_ RequestFeatures, snap *BackendSnapshot) (RouteDecision, error) {
	e := eligible(snap)
	if len(e) == 0 {
		return RouteDecision{}, ErrNoEligibleBackend
	}
	best := e[0]
	for _, b := range e[1:] {
		if b.RuntimeWaiting < best.RuntimeWaiting {
			best = b
		}
	}
	return RouteDecision{BackendID: best.Backend.ID, PolicyName: "least_runtime_waiting", PolicyVersion: "1",
		Reason: "min(vllm_num_requests_waiting)"}, nil
}

// HealthGatedLeastPressurePolicy combines health gating with a pressure score (queue + runtime + KV).
type HealthGatedLeastPressurePolicy struct{}

func (p *HealthGatedLeastPressurePolicy) SelectBackend(_ RequestFeatures, snap *BackendSnapshot) (RouteDecision, error) {
	e := eligible(snap)
	if len(e) == 0 {
		return RouteDecision{}, ErrNoEligibleBackend
	}
	pressure := func(b BackendState) float64 {
		// lower is better. queue depth + inflight + runtime waiting + KV pressure, de-weighted by stability.
		p := float64(b.QueueDepth+b.QueueInflight) + b.RuntimeWaiting + b.RuntimeRunning + 4*b.KVCacheUtil
		if b.StabilityScore > 0 {
			p /= b.StabilityScore
		}
		return p
	}
	best := e[0]
	bp := pressure(best)
	for _, b := range e[1:] {
		if cp := pressure(b); cp < bp {
			best, bp = b, cp
		}
	}
	return RouteDecision{BackendID: best.Backend.ID, PolicyName: "health_gated_least_pressure", PolicyVersion: "1",
		Reason: "min(pressure=queue+runtime+kv / stability)"}, nil
}

// PolicyByName returns a baseline policy by name.
func PolicyByName(name string) RoutingPolicy {
	switch name {
	case "round_robin":
		return &RoundRobinPolicy{}
	case "least_queued":
		return &LeastQueuedPolicy{}
	case "least_runtime_waiting":
		return &LeastRuntimeWaitingPolicy{}
	case "health_gated_least_pressure":
		return &HealthGatedLeastPressurePolicy{}
	default:
		return &LeastQueuedPolicy{}
	}
}
