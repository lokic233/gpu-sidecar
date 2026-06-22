package engine

import (
	"sync"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/adapters"
	"github.com/lokic233/gpu-sidecar/internal/core"
)

// rapidRestartSec: a worker that disappears and reappears within this window is counted as a
// rapid-restart (flapping) event — the only worker signal (besides confirmed evidence the host
// sidecar lacks) that penalizes stability. See worker_event_semantics.md.
const rapidRestartSec = 10.0

// deviceState bundles the per-device runtime objects.
type deviceState struct {
	identity core.Identity
	machine  *core.LifecycleMachine
	stab     *core.StabilityCalc
	hist     *core.DeviceHistory
	// for disconnect/rejoin detection (offline edge tracking)
	wasOffline  bool
	offlineMono time.Duration
	lastStatus  core.DeviceStatus

	// readiness/freshness tracking
	collected      bool          // at least one collection cycle completed
	lastProbeOK    bool          // most recent probe (visible+accessible) succeeded
	lastSampleWall time.Time     // wall time of last successful sample (telemetry freshness)
	lastSampleMono time.Duration // monotonic time of last successful sample

	// honest worker tracking
	workerSeen     bool       // have we ever observed a worker present
	workerEvents   *workerEventLog // bounded, time-windowed disappearance/appearance log
}

// workerEventLog is a bounded, time-windowed record of worker appearance/disappearance events.
// Used for NEUTRAL observability and rapid-restart detection. Memory is bounded by BOTH a max
// size (ring) AND a max age (window pruning). See worker_event_semantics.md / bounded-history.
type workerEventLog struct {
	disappearances []time.Duration // monotonic timestamps (pruned)
	appearances    []time.Duration // monotonic timestamps (pruned)
	windowSec      float64
	maxEntries     int
}

func newWorkerEventLog(windowSec float64, maxEntries int) *workerEventLog {
	return &workerEventLog{windowSec: windowSec, maxEntries: maxEntries}
}

func pruneTimes(s []time.Duration, now time.Duration, windowSec float64, maxEntries int) []time.Duration {
	cutoff := now - time.Duration(windowSec*float64(time.Second))
	// drop entries older than the window
	i := 0
	for i < len(s) && s[i] < cutoff {
		i++
	}
	s = s[i:]
	// cap size: keep the most recent maxEntries
	if len(s) > maxEntries {
		s = s[len(s)-maxEntries:]
	}
	return s
}

func (w *workerEventLog) recordDisappearance(now time.Duration) {
	w.disappearances = append(w.disappearances, now)
	w.disappearances = pruneTimes(w.disappearances, now, w.windowSec, w.maxEntries)
}

func (w *workerEventLog) recordAppearance(now time.Duration) {
	w.appearances = append(w.appearances, now)
	w.appearances = pruneTimes(w.appearances, now, w.windowSec, w.maxEntries)
}

// disappearancesInWindow returns the count of NEUTRAL disappearances in the recent window.
func (w *workerEventLog) disappearancesInWindow(now time.Duration) int {
	w.disappearances = pruneTimes(w.disappearances, now, w.windowSec, w.maxEntries)
	return len(w.disappearances)
}

// rapidRestartEvents counts disappearance→appearance pairs that occurred within rapidRestartSec of
// each other in the window — an instability signal (a worker flapping). This is the only worker
// signal (besides confirmed evidence, which the host sidecar lacks) that penalizes stability.
func (w *workerEventLog) rapidRestartEvents(now time.Duration, rapidRestartSec float64) int {
	w.disappearances = pruneTimes(w.disappearances, now, w.windowSec, w.maxEntries)
	w.appearances = pruneTimes(w.appearances, now, w.windowSec, w.maxEntries)
	thresh := time.Duration(rapidRestartSec * float64(time.Second))
	count := 0
	for _, d := range w.disappearances {
		for _, a := range w.appearances {
			if a > d && a-d <= thresh {
				count++
				break
			}
		}
	}
	return count
}

// Supervisor runs the probe loop and maintains normalized state for one host.
type Supervisor struct {
	mu        sync.RWMutex
	adapter   adapters.Adapter
	devices   map[string]*deviceState
	order     []string

	instanceID string
	hostname   string
	bootID     string
	version    string

	startMono time.Time // monotonic anchor (time.Now carries monotonic reading)
	startWall time.Time

	lifeCfg core.LifecycleConfig
	stabCfg core.StabilityConfig
	winSec  float64
	probeRingCap, pointRingCap, eventRingCap int

	// worker lifecycle tracking: deviceID -> set of known pids
	knownPIDs map[string]map[int]bool

	probeTimeout time.Duration
	accessEach   bool

	maxTelemetryAge time.Duration // readiness: max age of a successful sample
	pollInterval    time.Duration // for collector-stall detection

	// clock is injectable for deterministic tests. nowFn returns wall time; monoFn returns a
	// monotonic-style duration since start. Both default to real time.
	nowFn  func() time.Time
	monoFn func() time.Duration
}

func NewSupervisor(a adapters.Adapter, instanceID, hostname, bootID, version string,
	lifeCfg core.LifecycleConfig, stabCfg core.StabilityConfig, winSec float64,
	probeRingCap, pointRingCap, eventRingCap int, probeTimeout time.Duration, accessEach bool) *Supervisor {
	return &Supervisor{
		adapter: a, devices: map[string]*deviceState{},
		instanceID: instanceID, hostname: hostname, bootID: bootID, version: version,
		startMono: time.Now(), startWall: time.Now(),
		lifeCfg: lifeCfg, stabCfg: stabCfg, winSec: winSec,
		probeRingCap: probeRingCap, pointRingCap: pointRingCap, eventRingCap: eventRingCap,
		knownPIDs: map[string]map[int]bool{}, probeTimeout: probeTimeout, accessEach: accessEach,
		maxTelemetryAge: 15 * time.Second, pollInterval: 2 * time.Second,
	}
}

// SetClock injects deterministic time sources for testing.
func (s *Supervisor) SetClock(now func() time.Time, mono func() time.Duration) {
	s.nowFn = now
	s.monoFn = mono
}

func (s *Supervisor) wallNow() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// SetPollInterval informs the supervisor of the poll cadence (for stall detection).
func (s *Supervisor) SetPollInterval(d time.Duration) { s.pollInterval = d }

func (s *Supervisor) mono() time.Duration {
	if s.monoFn != nil {
		return s.monoFn()
	}
	return time.Since(s.startMono)
}

// Init discovers devices and sets up per-device state. deviceFilter empty = all.
func (s *Supervisor) Init(deviceFilter []string) error {
	ids, err := s.adapter.Discover(s.probeTimeout)
	if err != nil {
		return err
	}
	filter := map[string]bool{}
	for _, d := range deviceFilter {
		filter[d] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if len(filter) > 0 && !filter[id.DeviceID] {
			continue
		}
		id.SidecarInstanceID = s.instanceID
		id.Hostname = s.hostname
		id.BootID = s.bootID
		id.SidecarVersion = s.version
		id.BackendID = s.hostname + "-gpu" + id.DeviceID
		ds := &deviceState{
			identity: id,
			machine:  core.NewLifecycleMachine(s.lifeCfg),
			stab:     core.NewStabilityCalc(s.stabCfg),
			hist:     core.NewDeviceHistory(s.probeRingCap, s.pointRingCap, s.eventRingCap, s.winSec),
			workerEvents: newWorkerEventLog(s.winSec, s.eventRingCap),
		}
		ds.hist.MarkSidecarStart()
		s.devices[id.DeviceID] = ds
		s.order = append(s.order, id.DeviceID)
		s.knownPIDs[id.DeviceID] = map[int]bool{}
	}
	return nil
}

// PollOnce runs a single probe cycle across all devices.
func (s *Supervisor) PollOnce() {
	s.mu.RLock()
	order := append([]string(nil), s.order...)
	s.mu.RUnlock()
	for _, devID := range order {
		s.pollDevice(devID)
	}
}

func (s *Supervisor) pollDevice(devID string) {
	s.mu.RLock()
	ds := s.devices[devID]
	s.mu.RUnlock()
	if ds == nil {
		return
	}
	mono := s.mono()
	wall := s.wallNow()

	health, _ := s.adapter.Sample(devID, s.probeTimeout)
	access := health.GPUVisible
	if s.accessEach {
		access = s.adapter.AccessProbe(devID, s.probeTimeout)
	}
	health.GPUAccessible = access
	probeOK := health.GPUVisible && access

	ds.hist.RecordProbe(probeOK, health.ProbeLatencyMs, mono, wall)
	health.HeartbeatOK = true // sidecar itself is alive if we got here

	// Classify failure: hard (device gone / adapter init) vs soft (timeout / one access fail / malformed).
	// If the sample itself classified a failure, use it; otherwise, an access-probe-only failure is SOFT.
	var hardEvidence, softFailure bool
	failReason := ""
	switch health.ProbeFailure.Class {
	case core.FailureHard:
		hardEvidence = true
		failReason = health.ProbeFailure.Reason
	case core.FailureSoft:
		softFailure = true
		failReason = health.ProbeFailure.Reason
	default:
		if !access {
			// sample succeeded but active access probe failed => transient soft failure
			softFailure = true
			failReason = core.ReasonGPUAccessProbeFail
		}
	}

	// uncorrectable error delta
	curUncorr := uint64(0)
	uncorrSup := false
	if health.ECCUncorrectable.Supported {
		curUncorr, uncorrSup = health.ECCUncorrectable.Value, true
	} else if health.RASUncorrectable.Supported {
		curUncorr, uncorrSup = health.RASUncorrectable.Value, true
	}
	newUncorr := ds.hist.ObserveUncorr(curUncorr, uncorrSup)

	rel := ds.hist.Snapshot()

	// stability inputs (note: process-crash input intentionally removed — see worker_event_semantics.md)
	thrVar := -1.0
	if rel.ThroughputVariance.Supported {
		thrVar = rel.ThroughputVariance.Value
	}
	stabIn := core.StabilityInputs{
		RecentAvailability:  rel.RecentAvailability,
		ConsecutiveFailures: rel.ConsecutiveFailures,
		DisconnectsInWindow: ds.hist.DisconnectsInWindow(mono),
		LastRecoveryMs:      rel.LastRecoveryMs,
		ErrorCountDelta:     newUncorr,
		LatencyP50Ms:        rel.ProbeLatencyP50Ms,
		LatencyP95Ms:        rel.ProbeLatencyP95Ms,
		ThroughputVariance:  thrVar,
		// Neutral observation: unknown disappearances do NOT penalize the score.
		WorkerDisappearancesObserved: ds.workerEvents.disappearancesInWindow(mono),
		// Only rapid restart loops penalize (the host sidecar has no confirmed-abnormal-exit/OOM source).
		RapidRestartEvents: ds.workerEvents.rapidRestartEvents(mono, rapidRestartSec),
	}
	stab := ds.stab.Update(stabIn, wall)

	// lifecycle step
	freeRatio := health.EffectiveFreeMemRatio
	o := core.LifecycleObservation{
		ProbeOK:             probeOK,
		GPUVisible:          health.GPUVisible,
		GPUAccessible:       access,
		HardOfflineEvidence: hardEvidence,
		SoftFailure:         softFailure,
		ProbeFailReason:     failReason,
		UtilPct:             health.UtilizationGPU.Value,
		UtilSupported:       health.UtilizationGPU.Supported,
		FreeMemRatio:        freeRatio,
		StabilityScore:      stab.Score,
		NewUncorrErrors:     newUncorr,
		Mono:                mono,
	}
	prevState := ds.machine.State()
	newState, transitioned := ds.machine.Step(o)
	lifeInfo := ds.machine.Info()

	// disconnect / rejoin edge detection
	if newState == core.StateOffline && !ds.wasOffline {
		ds.wasOffline = true
		ds.offlineMono = mono
		ds.hist.MarkDisconnect(mono)
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: core.EventDisconnect,
			Detail: "device entered OFFLINE", ReasonCodes: lifeInfo.ReasonCodes,
			Evidence: map[string]any{"hard_offline": lifeInfo.HardOffline, "consecutive_soft_failures": lifeInfo.ConsecutiveSoftFailures}})
	}
	if ds.wasOffline && newState == core.StateRecovering {
		recMs := float64((mono - ds.offlineMono).Milliseconds())
		ds.hist.MarkRejoin(recMs)
		ds.wasOffline = false
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: core.EventRejoin,
			Detail: "probe recovered, entering RECOVERING", To: core.StateRecovering,
			Evidence: map[string]any{"recovery_duration_ms": recMs}})
	}

	if transitioned {
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: core.EventStateTransition,
			From: prevState, To: newState, Detail: joinReasons(lifeInfo.ReasonCodes), ReasonCodes: lifeInfo.ReasonCodes})
	}
	if !probeOK {
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: core.EventProbeFailure,
			Detail: probeFailDetail(health), ReasonCodes: []string{failReason},
			Evidence: map[string]any{"class": health.ProbeFailure.Class}})
	}

	// worker lifecycle detection via compute proc count (evidence-only; cause = unknown)
	s.detectWorkers(ds, devID, health, wall, mono)

	// capacity HINT: explicitly heuristic, host-derived. NOT serving capacity.
	cap := computeCapacityHint(health, stab.Score)

	status := core.DeviceStatus{
		Identity: ds.identity, Health: health, LifecycleState: newState,
		Lifecycle: lifeInfo, Reliability: ds.hist.Snapshot(), Stability: stab, Capacity: cap,
	}


	// add time-series point
	ds.hist.AddPoint(core.HistoryPoint{
		Timestamp: wall, DeviceID: devID, LifecycleState: newState, StabilityScore: stab.Score,
		UtilGPU: health.UtilizationGPU.Value, MemUsedBytes: health.MemUsedBytes.Value,
		TemperatureC: health.TemperatureC.Value, ProbeOK: probeOK, ProbeLatencyMs: health.ProbeLatencyMs,
	})

	s.mu.Lock()
	ds.collected = true
	ds.lastProbeOK = probeOK
	if probeOK {
		ds.lastSampleWall = wall
		ds.lastSampleMono = mono
	}
	ds.lastStatus = status

	s.mu.Unlock()
}

// detectWorkers tracks compute-process count changes as EVIDENCE-ONLY worker events.
// A pure host sidecar observes count/memory deltas; it CANNOT prove crash vs graceful exit vs
// OOM from those deltas alone. So a disappearance is reported as WORKER_DISAPPEARED with
// termination_cause=unknown, never as a confirmed crash. See worker_event_semantics.md.
func (s *Supervisor) detectWorkers(ds *deviceState, devID string, h core.Health, wall time.Time, mono time.Duration) {
	if !h.ComputeProcs.Supported {
		return
	}
	prev := ds.lastStatus.Health.ComputeProcs
	if !prev.Supported {
		// first observation with proc support: record presence if any
		if h.ComputeProcs.Value > 0 {
			ds.workerSeen = true
			ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: core.EventWorkerObserved,
				Detail: "compute process present", Evidence: map[string]any{"process_count": h.ComputeProcs.Value}})
		}
		return
	}
	memReleased := int64(0)
	if prev.Value > h.ComputeProcs.Value && ds.lastStatus.Health.MemUsedBytes.Supported && h.MemUsedBytes.Supported {
		memReleased = int64(ds.lastStatus.Health.MemUsedBytes.Value) - int64(h.MemUsedBytes.Value)
	}
	if h.ComputeProcs.Value > prev.Value {
		for i := 0; i < h.ComputeProcs.Value-prev.Value; i++ {
			ds.hist.MarkWorkerStart()
		}
		ds.workerSeen = true
		ds.workerEvents.recordAppearance(mono)
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: core.EventWorkerStarted,
			Detail: "compute process count increased",
			Evidence: map[string]any{"previous_process_count": prev.Value, "current_process_count": h.ComputeProcs.Value}})
	} else if h.ComputeProcs.Value < prev.Value {
		for i := 0; i < prev.Value-h.ComputeProcs.Value; i++ {
			ds.hist.MarkWorkerStop()
		}
		ds.workerEvents.recordDisappearance(mono)
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: core.EventWorkerDisappeared,
			Detail: "compute process count decreased (cause NOT observable from host signals — neutral)",
			TerminationCause: core.CauseUnknown, GroundTruthSource: "",
			Evidence: map[string]any{
				"previous_process_count": prev.Value,
				"current_process_count":  h.ComputeProcs.Value,
				"memory_released_bytes":  memReleased,
			}})
	}
}

// joinReasons makes a short human detail from reason codes.
func joinReasons(codes []string) string {
	if len(codes) == 0 {
		return "healthy"
	}
	out := codes[0]
	for _, c := range codes[1:] {
		out += "," + c
	}
	return out
}

// computeCapacityHint builds the explicitly-heuristic host capacity hint.
func computeCapacityHint(h core.Health, stability float64) core.CapacityHint {
	freeRatio := h.EffectiveFreeMemRatio
	utilHeadroom := 1.0
	if h.UtilizationGPU.Supported {
		utilHeadroom = 1.0 - h.UtilizationGPU.Value/100.0
	}
	hint := core.Clamp01(freeRatio * utilHeadroom * stability)
	return core.CapacityHint{
		HostCapacityHint:  hint,
		CapacitySemantics: "heuristic_host_derived",
		Components: map[string]float64{
			"free_memory_ratio":   core.Clamp01(freeRatio),
			"utilization_headroom": core.Clamp01(utilHeadroom),
			"stability_score":     core.Clamp01(stability),
		},
		RuntimeServingCapacitySupported: false,
		RuntimeServingCapacity:          nil,
	}
}


func probeFailDetail(h core.Health) string {
	if len(h.UnsupportedFields) > 0 {
		return h.UnsupportedFields[len(h.UnsupportedFields)-1]
	}
	return "probe failed"
}

// SetDraining toggles drain on a device. Records a lifecycle event with prev/new draining state
// and the request source. Idempotent: repeated identical requests are no-ops (changed=false).
// Returns (found, changed).
func (s *Supervisor) SetDraining(devID string, d bool, source string) (found bool, changed bool) {
	s.mu.RLock()
	ds := s.devices[devID]
	s.mu.RUnlock()
	if ds == nil {
		return false, false
	}
	prevState, changed := ds.machine.SetDrainingChecked(d)
	if !changed {
		return true, false // idempotent no-op
	}
	ds.hist.AddEvent(core.Event{
		Timestamp: time.Now(), DeviceID: devID, Kind: core.EventStateTransition,
		From: prevState, To: prevState, // actual state recomputed next poll
		Detail:      "operator drain change",
		ReasonCodes: []string{core.ReasonOperatorDrain},
		Evidence: map[string]any{
			"draining_previous": !d, "draining_new": d, "request_source": source,
		},
	})
	return true, true
}

// Draining reports whether the given device is currently operator-drained.
func (s *Supervisor) Draining(devID string) bool {
	s.mu.RLock()
	ds := s.devices[devID]
	s.mu.RUnlock()
	if ds == nil {
		return false
	}
	return ds.machine.Draining()
}

// SetThroughputVariance records a controlled-probe CoV for a device.
func (s *Supervisor) SetThroughputVariance(devID string, cov float64) {
	s.mu.RLock()
	ds := s.devices[devID]
	s.mu.RUnlock()
	if ds != nil {
		ds.hist.SetThroughputVariance(cov)
	}
}

// core.HostStatus assembles the full normalized host status.
func (s *Supervisor) HostStatus() core.HostStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hs := core.HostStatus{
		SidecarInstanceID: s.instanceID, Hostname: s.hostname, Vendor: s.adapter.Vendor(),
		SidecarVersion: s.version, BootID: s.bootID, Timestamp: s.wallNow(),
		UptimeSeconds: time.Since(s.startWall).Seconds(),
	}
	for _, devID := range s.order {
		hs.Devices = append(hs.Devices, s.devices[devID].lastStatus)
	}
	return hs
}

// History returns bounded history points for all devices.
func (s *Supervisor) History() []core.HistoryPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []core.HistoryPoint
	for _, devID := range s.order {
		out = append(out, s.devices[devID].hist.Points()...)
	}
	return out
}

// Events returns bounded events for all devices.
func (s *Supervisor) Events() []core.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []core.Event
	for _, devID := range s.order {
		out = append(out, s.devices[devID].hist.Events()...)
	}
	return out
}

// MaxTelemetryAge bounds how stale a successful sample may be for readiness. Configurable.
func (s *Supervisor) SetMaxTelemetryAge(d time.Duration) { s.maxTelemetryAge = d }

// ReadinessResult is the structured outcome of a readiness evaluation.
type ReadinessResult struct {
	// ControlPlaneReady is host-level readiness: the sidecar has collected, is not stalled, and can
	// provide trustworthy status for AT LEAST ONE managed device. This is NOT proof that every GPU
	// can receive traffic — inspect per-device fields / details for that.
	ControlPlaneReady bool `json:"control_plane_ready"`
	AnyDeviceReady    bool `json:"any_device_ready"`
	AllDevicesReady   bool `json:"all_devices_ready"`
	ReadyDeviceCount  int  `json:"ready_device_count"`
	TotalDeviceCount  int  `json:"total_device_count"`

	// Ready mirrors ControlPlaneReady for backward compatibility with round-2 consumers.
	Ready        bool     `json:"ready"`
	ReadyDevices int      `json:"ready_devices"` // deprecated alias of ReadyDeviceCount
	TotalDevices int      `json:"total_devices"` // deprecated alias of TotalDeviceCount
	Reasons      []string `json:"reasons"`       // why control-plane NOT ready (empty if ready)
	Details      []DeviceReadiness `json:"details"`
}

// DeviceReadiness is per-device readiness with reasons.
type DeviceReadiness struct {
	DeviceID       string   `json:"device_id"`
	Ready          bool     `json:"ready"`
	LifecycleState string   `json:"lifecycle_state"`
	TelemetryAgeMs int64    `json:"telemetry_age_ms"`
	Reasons        []string `json:"reasons"`
}

// Readiness implements the precise /readyz contract. A device is "ready" (trustworthy to inspect)
// only if ALL hold:
//   - at least one collection cycle completed
//   - GPU visible AND accessible
//   - latest required probe succeeded
//   - telemetry age below max
//   - lifecycle state is not OFFLINE
//   - sidecar collector not internally unhealthy (poll loop running)
//
// DEGRADED/BUSY/DRAINING/RECOVERING are READY for *inspection* purposes (the sidecar can still
// truthfully report them); whether the backend should receive TRAFFIC is a separate decision the
// lifecycle_state conveys. See readiness_semantics.md.
func (s *Supervisor) Readiness(now time.Time) ReadinessResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := ReadinessResult{TotalDeviceCount: len(s.order), TotalDevices: len(s.order)}
	collectorStalled := s.collectorStalled(now)
	for _, devID := range s.order {
		dr := s.deviceReadinessLocked(devID, now, collectorStalled)
		if dr.Ready {
			res.ReadyDeviceCount++
		}
		res.Details = append(res.Details, dr)
	}
	res.ReadyDevices = res.ReadyDeviceCount // deprecated alias
	res.AnyDeviceReady = res.ReadyDeviceCount > 0
	res.AllDevicesReady = res.TotalDeviceCount > 0 && res.ReadyDeviceCount == res.TotalDeviceCount
	res.ControlPlaneReady = res.AnyDeviceReady && !collectorStalled
	res.Ready = res.ControlPlaneReady // backward-compat alias
	if !res.ControlPlaneReady {
		if collectorStalled {
			res.Reasons = append(res.Reasons, "COLLECTOR_STALLED")
		}
		seen := map[string]bool{"COLLECTOR_STALLED": collectorStalled}
		for _, d := range res.Details {
			for _, r := range d.Reasons {
				if !seen[r] {
					seen[r] = true
					res.Reasons = append(res.Reasons, r)
				}
			}
		}
		if len(res.Reasons) == 0 {
			res.Reasons = []string{"NO_READY_DEVICES"}
		}
	}
	return res
}

// DeviceReadiness returns readiness for a single managed device. found=false if devID is unmanaged.
func (s *Supervisor) DeviceReadiness(devID string, now time.Time) (DeviceReadiness, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.devices[devID]; !ok {
		return DeviceReadiness{}, false
	}
	return s.deviceReadinessLocked(devID, now, s.collectorStalled(now)), true
}

// deviceReadinessLocked computes one device's readiness. Caller must hold s.mu.
func (s *Supervisor) deviceReadinessLocked(devID string, now time.Time, collectorStalled bool) DeviceReadiness {
	ds := s.devices[devID]
	dr := DeviceReadiness{DeviceID: devID, LifecycleState: string(ds.lastStatus.LifecycleState)}
	var reasons []string
	if !ds.collected {
		reasons = append(reasons, "NO_COLLECTION_YET")
	}
	if !ds.lastStatus.Health.GPUVisible {
		reasons = append(reasons, "GPU_NOT_VISIBLE")
	}
	if !ds.lastStatus.Health.GPUAccessible {
		reasons = append(reasons, "GPU_NOT_ACCESSIBLE")
	}
	if !ds.lastProbeOK {
		reasons = append(reasons, "LAST_PROBE_FAILED")
	}
	ageMs := int64(-1)
	if !ds.lastSampleWall.IsZero() {
		ageMs = now.Sub(ds.lastSampleWall).Milliseconds()
		if now.Sub(ds.lastSampleWall) > s.maxTelemetryAge {
			reasons = append(reasons, "TELEMETRY_STALE")
		}
	} else {
		reasons = append(reasons, "NO_SUCCESSFUL_SAMPLE")
	}
	dr.TelemetryAgeMs = ageMs
	if ds.lastStatus.LifecycleState == core.StateOffline {
		reasons = append(reasons, "LIFECYCLE_OFFLINE")
	}
	if collectorStalled {
		reasons = append(reasons, "COLLECTOR_STALLED")
	}
	dr.Reasons = reasons
	dr.Ready = len(reasons) == 0
	return dr
}

// collectorStalled reports whether the poll loop has missed updates (internal collector health).
// Stalled if the most recent sample across all devices is older than 3× the poll interval
// (and at least one collection has happened).
func (s *Supervisor) collectorStalled(now time.Time) bool {
	anyCollected := false
	var newest time.Time
	for _, devID := range s.order {
		ds := s.devices[devID]
		if ds.collected {
			anyCollected = true
		}
		if ds.lastStatus.Health.Timestamp.After(newest) {
			newest = ds.lastStatus.Health.Timestamp
		}
	}
	if !anyCollected || newest.IsZero() {
		return true // no collection completed => collector not yet healthy
	}
	limit := 3 * s.pollInterval
	if limit < 6*time.Second {
		limit = 6 * time.Second
	}
	return now.Sub(newest) > limit
}

// Ready is the simple boolean form of Readiness (kept for compatibility).
func (s *Supervisor) Ready() bool { return s.Readiness(time.Now()).Ready }

func (s *Supervisor) DeviceCount() int { s.mu.RLock(); defer s.mu.RUnlock(); return len(s.order) }

