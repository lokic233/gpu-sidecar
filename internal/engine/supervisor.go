package engine

import (
	"sync"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/adapters"
	"github.com/lokic233/gpu-sidecar/internal/core"
)

// deviceState bundles the per-device runtime objects.
type deviceState struct {
	identity core.Identity
	machine  *core.LifecycleMachine
	stab     *core.StabilityCalc
	hist     *core.DeviceHistory
	// for disconnect/rejoin detection (offline edge tracking)
	wasOffline   bool
	offlineMono  time.Duration
	lastStatus   core.DeviceStatus
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
	}
}

func (s *Supervisor) mono() time.Duration { return time.Since(s.startMono) }

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
	wall := time.Now()

	health, _ := s.adapter.Sample(devID, s.probeTimeout)
	access := health.GPUVisible
	if s.accessEach {
		access = s.adapter.AccessProbe(devID, s.probeTimeout)
	}
	health.GPUAccessible = access
	probeOK := health.GPUVisible && access

	ds.hist.RecordProbe(probeOK, health.ProbeLatencyMs, mono, wall)
	health.HeartbeatOK = true // sidecar itself is alive if we got here

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

	// stability inputs
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
	}
	stab := ds.stab.Update(stabIn, wall)

	// lifecycle step
	freeRatio := health.EffectiveFreeMemRatio
	o := core.LifecycleObservation{
		ProbeOK:             probeOK,
		GPUVisible:          health.GPUVisible,
		GPUAccessible:       access,
		ConsecutiveFailures: rel.ConsecutiveFailures,
		UtilPct:             health.UtilizationGPU.Value,
		UtilSupported:       health.UtilizationGPU.Supported,
		FreeMemRatio:        freeRatio,
		StabilityScore:      stab.Score,
		NewUncorrErrors:     newUncorr,
		Mono:                mono,
	}
	prevState := ds.machine.State()
	newState, transitioned := ds.machine.Step(o)

	// disconnect / rejoin edge detection
	if newState == core.StateOffline && !ds.wasOffline {
		ds.wasOffline = true
		ds.offlineMono = mono
		ds.hist.MarkDisconnect(mono)
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: "DISCONNECT", Detail: "probe/access failure"})
	}
	if ds.wasOffline && newState == core.StateRecovering {
		recMs := float64((mono - ds.offlineMono).Milliseconds())
		ds.hist.MarkRejoin(recMs)
		ds.wasOffline = false
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: "REJOIN", Detail: "probe recovered", To: core.StateRecovering})
	}

	if transitioned {
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: "STATE_TRANSITION", From: prevState, To: newState,
			Detail: stateReason(o, stab.Score)})
	}
	if !probeOK {
		ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: "PROBE_FAILURE", Detail: probeFailDetail(health)})
	}

	// worker lifecycle detection via compute proc pids
	s.detectWorkers(ds, devID, health, wall)

	// effective capacity = free-mem-ratio scaled by stability and (1-util)
	effCap := freeRatio
	if health.UtilizationGPU.Supported {
		effCap = freeRatio * (1.0 - health.UtilizationGPU.Value/100.0)
	}
	effCap = core.Clamp01(effCap * stab.Score)

	status := core.DeviceStatus{
		Identity: ds.identity, Health: health, LifecycleState: newState,
		Reliability: ds.hist.Snapshot(), Stability: stab, EffectiveCapacity: effCap,
	}

	// add time-series point
	ds.hist.AddPoint(core.HistoryPoint{
		Timestamp: wall, DeviceID: devID, LifecycleState: newState, StabilityScore: stab.Score,
		UtilGPU: health.UtilizationGPU.Value, MemUsedBytes: health.MemUsedBytes.Value,
		TemperatureC: health.TemperatureC.Value, ProbeOK: probeOK, ProbeLatencyMs: health.ProbeLatencyMs,
	})

	s.mu.Lock()
	ds.lastStatus = status
	s.mu.Unlock()
}

// detectWorkers tracks compute-process count changes as worker start/stop events.
// Note: we observe count deltas (vendor tools don't always give per-device pids reliably).
func (s *Supervisor) detectWorkers(ds *deviceState, devID string, h core.Health, wall time.Time) {
	if !h.ComputeProcs.Supported {
		return
	}
	prev := ds.lastStatus.Health.ComputeProcs
	if !prev.Supported {
		return
	}
	if h.ComputeProcs.Value > prev.Value {
		for i := 0; i < h.ComputeProcs.Value-prev.Value; i++ {
			ds.hist.MarkWorkerStart()
			ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: "WORKER_START", Detail: "compute proc count increased"})
		}
	} else if h.ComputeProcs.Value < prev.Value {
		for i := 0; i < prev.Value-h.ComputeProcs.Value; i++ {
			ds.hist.MarkWorkerStop()
			ds.hist.AddEvent(core.Event{Timestamp: wall, DeviceID: devID, Kind: "WORKER_STOP", Detail: "compute proc count decreased"})
		}
	}
}

func stateReason(o core.LifecycleObservation, score float64) string {
	switch {
	case !o.GPUVisible:
		return "gpu not visible"
	case !o.GPUAccessible:
		return "gpu not accessible"
	case o.NewUncorrErrors > 0:
		return "uncorrectable errors observed"
	case score < 0.55:
		return "low stability score"
	case o.UtilSupported && o.UtilPct >= 80:
		return "high utilization"
	case o.FreeMemRatio <= 0.10:
		return "low free memory"
	default:
		return "healthy"
	}
}

func probeFailDetail(h core.Health) string {
	if len(h.UnsupportedFields) > 0 {
		return h.UnsupportedFields[len(h.UnsupportedFields)-1]
	}
	return "probe failed"
}

// SetDraining toggles drain on a device.
func (s *Supervisor) SetDraining(devID string, d bool) bool {
	s.mu.RLock()
	ds := s.devices[devID]
	s.mu.RUnlock()
	if ds == nil {
		return false
	}
	ds.machine.SetDraining(d)
	return true
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
		SidecarVersion: s.version, BootID: s.bootID, Timestamp: time.Now(),
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

// Ready reports whether at least one device is currently inspectable.
func (s *Supervisor) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, devID := range s.order {
		st := s.devices[devID].lastStatus
		if st.Health.GPUVisible {
			return true
		}
	}
	return false
}

func (s *Supervisor) DeviceCount() int { s.mu.RLock(); defer s.mu.RUnlock(); return len(s.order) }
