package core

import (
	"sort"
	"sync"
	"time"
)

// ring is a fixed-capacity ring buffer.
type ring[T any] struct {
	buf  []T
	head int
	size int
	cap  int
}

func newRing[T any](capacity int) *ring[T] { return &ring[T]{buf: make([]T, capacity), cap: capacity} }
func (r *ring[T]) push(v T) {
	r.buf[r.head] = v
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}
func (r *ring[T]) items() []T {
	out := make([]T, 0, r.size)
	start := (r.head - r.size + r.cap) % r.cap
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[(start+i)%r.cap])
	}
	return out
}

// probeRec is a single probe outcome (monotonic-based latency).
type probeRec struct {
	mono    time.Duration
	ok      bool
	latency float64
}

// DeviceHistory aggregates bounded history + reliability accounting for one device.
type DeviceHistory struct {
	mu sync.Mutex

	probes  *ring[probeRec]
	points  *ring[HistoryPoint]
	events  *ring[Event]

	rel        Reliability
	lastUncorr uint64
	haveUncorr bool

	// disconnect/rejoin window tracking (timestamps, monotonic)
	disconnects *ring[time.Duration]
	windowSec   float64
}

func NewDeviceHistory(probeCap, pointCap, eventCap int, windowSec float64) *DeviceHistory {
	return &DeviceHistory{
		probes:      newRing[probeRec](probeCap),
		points:      newRing[HistoryPoint](pointCap),
		events:      newRing[Event](eventCap),
		disconnects: newRing[time.Duration](64),
		windowSec:   windowSec,
	}
}

// RecordProbe records a probe outcome and updates reliability counters. Uses monotonic time
// for durations and wall time only for human-facing timestamps.
func (d *DeviceHistory) RecordProbe(ok bool, latencyMs float64, mono time.Duration, wall time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probes.push(probeRec{mono: mono, ok: ok, latency: latencyMs})
	if ok {
		t := wall
		d.rel.LastSuccessfulProbe = &t
		d.rel.ConsecutiveFailures = 0
	} else {
		t := wall
		d.rel.LastFailedProbe = &t
		d.rel.ConsecutiveFailures++
	}
	d.recomputeWindow(mono)
}

// recomputeWindow recomputes availability/failure-rate and latency percentiles over the window.
func (d *DeviceHistory) recomputeWindow(now time.Duration) {
	items := d.probes.items()
	var total, okc int
	var lats []float64
	cutoff := now - time.Duration(d.windowSec*float64(time.Second))
	for _, p := range items {
		if p.mono < cutoff {
			continue
		}
		total++
		if p.ok {
			okc++
			lats = append(lats, p.latency)
		}
	}
	if total > 0 {
		d.rel.RecentAvailability = float64(okc) / float64(total)
		d.rel.RecentFailureRate = float64(total-okc) / float64(total)
	}
	if len(lats) > 0 {
		sort.Float64s(lats)
		d.rel.ProbeLatencyP50Ms = percentile(lats, 50)
		d.rel.ProbeLatencyP95Ms = percentile(lats, 95)
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := (p / 100.0) * float64(len(sorted)-1)
	lo := int(rank)
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
}

// ObserveUncorr returns the delta of new uncorrectable errors since last observation.
func (d *DeviceHistory) ObserveUncorr(cur uint64, supported bool) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !supported {
		return 0
	}
	if !d.haveUncorr {
		d.lastUncorr, d.haveUncorr = cur, true
		return 0
	}
	var delta uint64
	if cur > d.lastUncorr {
		delta = cur - d.lastUncorr
	}
	d.lastUncorr = cur
	return delta
}

func (d *DeviceHistory) AddPoint(p HistoryPoint)  { d.mu.Lock(); d.points.push(p); d.mu.Unlock() }
func (d *DeviceHistory) AddEvent(e Event)         { d.mu.Lock(); d.events.push(e); d.mu.Unlock() }
func (d *DeviceHistory) Points() []HistoryPoint   { d.mu.Lock(); defer d.mu.Unlock(); return d.points.items() }
func (d *DeviceHistory) Events() []Event          { d.mu.Lock(); defer d.mu.Unlock(); return d.events.items() }

func (d *DeviceHistory) MarkWorkerStart() { d.mu.Lock(); d.rel.WorkerStarts++; d.mu.Unlock() }
func (d *DeviceHistory) MarkWorkerStop()  { d.mu.Lock(); d.rel.WorkerStops++; d.mu.Unlock() }
func (d *DeviceHistory) MarkSidecarStart() { d.mu.Lock(); d.rel.SidecarStartCount++; d.mu.Unlock() }

func (d *DeviceHistory) MarkDisconnect(mono time.Duration) {
	d.mu.Lock(); defer d.mu.Unlock()
	d.rel.DisconnectCount++
	d.disconnects.push(mono)
}
func (d *DeviceHistory) MarkRejoin(recoveryMs float64) {
	d.mu.Lock(); defer d.mu.Unlock()
	d.rel.RejoinCount++
	d.rel.LastRecoveryMs = recoveryMs
}

// DisconnectsInWindow counts disconnects within the recent window.
func (d *DeviceHistory) DisconnectsInWindow(now time.Duration) int {
	d.mu.Lock(); defer d.mu.Unlock()
	cutoff := now - time.Duration(d.windowSec*float64(time.Second))
	c := 0
	for _, t := range d.disconnects.items() {
		if t >= cutoff {
			c++
		}
	}
	return c
}

// Snapshot returns a copy of the reliability struct.
func (d *DeviceHistory) Snapshot() Reliability { d.mu.Lock(); defer d.mu.Unlock(); return d.rel }

// SetThroughputVariance records controlled-probe throughput CoV.
func (d *DeviceHistory) SetThroughputVariance(cov float64) {
	d.mu.Lock(); defer d.mu.Unlock()
	d.rel.ThroughputVariance = Sup(cov)
}
