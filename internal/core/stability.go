package core

import (
	"math"
	"time"
)

// StabilityConfig parameterizes the stability score. All weights documented in
// artifacts/stability_score.md. Weights sum to 1.0.
type StabilityConfig struct {
	WAvailability float64 // recent availability ratio
	WFailures     float64 // consecutive failures penalty
	WDisconnect   float64 // disconnect frequency penalty
	WRecovery     float64 // recovery duration penalty
	WErrors       float64 // vendor error counters penalty
	WLatency      float64 // probe latency instability penalty
	WThroughput   float64 // controlled throughput variance penalty (when enabled)

	// Asymmetric EWMA: instability drops fast, recovery is slow ("memory of instability").
	AlphaUp   float64 // smoothing when new instantaneous score < current (fast drop)
	AlphaDown float64 // smoothing when new instantaneous score > current (slow recovery)
}

func DefaultStabilityConfig() StabilityConfig {
	return StabilityConfig{
		WAvailability: 0.30,
		WFailures:     0.20,
		WDisconnect:   0.15,
		WRecovery:     0.10,
		WErrors:       0.10,
		WLatency:      0.10,
		WThroughput:   0.05,
		AlphaUp:       0.60, // react quickly to instability
		AlphaDown:     0.08, // recover slowly: ~ requires sustained health
	}
}

// StabilityCalc holds the smoothed score state for one device.
type StabilityCalc struct {
	cfg     StabilityConfig
	smamooth float64
	started bool
}

func NewStabilityCalc(cfg StabilityConfig) *StabilityCalc {
	return &StabilityCalc{cfg: cfg, smamooth: 1.0}
}

// clamp01 bounds x to [0,1].
func Clamp01(x float64) float64 { return clamp01(x) }

func clamp01(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// Inputs captures the signals used to compute the instantaneous (pre-smoothing) score.
type StabilityInputs struct {
	RecentAvailability  float64 // [0,1]
	ConsecutiveFailures int
	DisconnectsInWindow int
	LastRecoveryMs      float64
	ErrorCountDelta     uint64  // new uncorrectable errors observed in window
	LatencyP95Ms        float64
	LatencyP50Ms        float64
	ThroughputVariance  float64 // coefficient of variation, when enabled; <0 = disabled

	// --- Worker signals ---
	// WorkerDisappearancesObserved counts worker processes that vanished in the recent window with
	// UNKNOWN cause. This is NEUTRAL by default: a disappearance may be a graceful stop, scale-down,
	// rolling replacement, SIGTERM, SIGKILL, crash, OOM, or eviction — the host sidecar cannot tell.
	// It does NOT reduce the stability score. It is carried only for observability/auditing.
	WorkerDisappearancesObserved int
	// ConfirmedAbnormalWorkerExits counts disappearances correlated with STRONGER evidence (non-zero
	// supervised exit, confirmed OOM, runtime health failure, vendor error, GPU access failure,
	// cgroup/container abnormal-termination event). These DO reduce the score.
	ConfirmedAbnormalWorkerExits int
	// ConfirmedOOMEvents counts confirmed out-of-memory terminations in the window (strong penalty).
	ConfirmedOOMEvents int
	// RapidRestartEvents counts rapid disappearance/restart loops in the window (instability signal).
	RapidRestartEvents int
}

// instantaneous computes the unsmoothed [0,1] score and its components.
func (s *StabilityCalc) instantaneous(in StabilityInputs) (float64, map[string]float64) {
	c := s.cfg

	avail := clamp01(in.RecentAvailability)

	// failures: 0 failures -> 1.0; decays ~ e^{-k*failures}
	failScore := math.Exp(-0.7 * float64(in.ConsecutiveFailures))

	// disconnects in window: each disconnect costs; 0 -> 1.0
	discScore := math.Exp(-0.5 * float64(in.DisconnectsInWindow))

	// recovery duration: fast recovery (<2s) ~1.0, decays over tens of seconds
	recScore := 1.0
	if in.LastRecoveryMs > 0 {
		recScore = math.Exp(-in.LastRecoveryMs / 30000.0) // 30s scale
	}

	// vendor errors: any new uncorrectable error is a strong penalty
	errScore := math.Exp(-1.5 * float64(in.ErrorCountDelta))

	// latency instability: ratio p95/p50; 1.0 stable -> 1.0 score, high ratio penalized
	latScore := 1.0
	if in.LatencyP50Ms > 0 && in.LatencyP95Ms > 0 {
		ratio := in.LatencyP95Ms / in.LatencyP50Ms
		latScore = clamp01(1.0 - (ratio-1.0)/9.0) // ratio>=10x => 0
	}

	// throughput variance (coefficient of variation). <0 means disabled -> neutral 1.0
	thrScore := 1.0
	if in.ThroughputVariance >= 0 {
		thrScore = clamp01(1.0 - in.ThroughputVariance*2.0) // CoV 0.5 => 0
	}

	// Worker termination penalty: ONLY confirmed-abnormal evidence and rapid-restart loops reduce
	// the score. Unknown disappearances (WorkerDisappearancesObserved) are NEUTRAL by default — a
	// disappearance alone (count/memory delta) is not evidence of a problem. See worker_event_semantics.md.
	if in.ConfirmedAbnormalWorkerExits > 0 {
		failScore *= math.Exp(-0.6 * float64(in.ConfirmedAbnormalWorkerExits))
	}
	if in.ConfirmedOOMEvents > 0 {
		failScore *= math.Exp(-0.8 * float64(in.ConfirmedOOMEvents)) // OOM is a strong instability signal
	}
	if in.RapidRestartEvents > 0 {
		failScore *= math.Exp(-0.4 * float64(in.RapidRestartEvents))
	}

	comps := map[string]float64{
		"availability": c.WAvailability * avail,
		"failures":     c.WFailures * failScore,
		"disconnect":   c.WDisconnect * discScore,
		"recovery":     c.WRecovery * recScore,
		"errors":       c.WErrors * errScore,
		"latency":      c.WLatency * latScore,
		"throughput":   c.WThroughput * thrScore,
	}
	raw := comps["availability"] + comps["failures"] + comps["disconnect"] +
		comps["recovery"] + comps["errors"] + comps["latency"] + comps["throughput"]
	return clamp01(raw), comps
}

// Update folds the new instantaneous score into the asymmetric EWMA and returns the result.
func (s *StabilityCalc) Update(in StabilityInputs, now time.Time) StabilityScore {
	inst, comps := s.instantaneous(in)
	if !s.started {
		s.smamooth = inst
		s.started = true
	} else if inst < s.smamooth {
		// instability: react fast
		s.smamooth = s.cfg.AlphaUp*inst + (1-s.cfg.AlphaUp)*s.smamooth
	} else {
		// recovery: react slowly (memory of recent instability)
		s.smamooth = s.cfg.AlphaDown*inst + (1-s.cfg.AlphaDown)*s.smamooth
	}
	comps["instantaneous"] = inst
	comps["smoothed"] = s.smamooth
	return StabilityScore{Score: clamp01(s.smamooth), Components: comps, UpdatedAt: now}
}

// Score returns the current smoothed score without updating.
func (s *StabilityCalc) Score() float64 { return clamp01(s.smamooth) }
