package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

// fakeClock is a controllable monotonic+wall clock.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	mono time.Duration
}

func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mono += d; c.mu.Unlock() }
func (c *fakeClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) monod() time.Duration    { c.mu.Lock(); defer c.mu.Unlock(); return c.mono }

func newTestSup(t *testing.T, mock *mockAdapter, devices ...string) (*Supervisor, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	sup := NewSupervisor(mock, "test-instance", "test-host", "boot-1", "test-ver",
		core.DefaultLifecycleConfig(), core.DefaultStabilityConfig(), 120,
		256, 256, 128, 2*time.Second, true)
	sup.SetClock(clk.now, clk.monod)
	sup.SetMaxTelemetryAge(15 * time.Second)
	sup.SetPollInterval(2 * time.Second)
	if err := sup.Init(devices); err != nil {
		t.Fatalf("init: %v", err)
	}
	return sup, clk
}

func poll(sup *Supervisor, clk *fakeClock, n int, step time.Duration) {
	for i := 0; i < n; i++ {
		clk.advance(step)
		sup.PollOnce()
	}
}

func TestSupervisor_SuccessfulCollection(t *testing.T) {
	mock := newMockAdapter("0", "1")
	sup, clk := newTestSup(t, mock, "0", "1")
	poll(sup, clk, 3, 2*time.Second)
	hs := sup.HostStatus()
	if len(hs.Devices) != 2 {
		t.Fatalf("want 2 devices, got %d", len(hs.Devices))
	}
	for _, d := range hs.Devices {
		if !d.Health.GPUVisible {
			t.Fatalf("dev %s should be visible", d.Identity.DeviceID)
		}
		if d.Capacity.CapacitySemantics != "heuristic_host_derived" {
			t.Fatalf("capacity semantics must be labeled heuristic, got %q", d.Capacity.CapacitySemantics)
		}
		if d.Capacity.RuntimeServingCapacitySupported {
			t.Fatal("runtime serving capacity must be false for host sidecar")
		}
	}
}

func TestSupervisor_ReadyWhenHealthy(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second)
	r := sup.Readiness(clk.now())
	if !r.Ready {
		t.Fatalf("should be ready, reasons=%v", r.Reasons)
	}
}

func TestSupervisor_NotReadyBeforeFirstCollection(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	// no PollOnce yet
	r := sup.Readiness(clk.now())
	if r.Ready {
		t.Fatal("must not be ready before first collection")
	}
}

func TestSupervisor_NotReadyWhenInaccessible(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second)
	// GPU still "visible" in last sample but access probe now fails => soft failure
	mock.setSoftFail("0")
	poll(sup, clk, 1, 2*time.Second)
	r := sup.Readiness(clk.now())
	if r.Ready {
		t.Fatalf("must not be ready when inaccessible, reasons=%v", r.Reasons)
	}
}

func TestSupervisor_NotReadyWhenStale(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second)
	if !sup.Readiness(clk.now()).Ready {
		t.Fatal("precondition: should be ready")
	}
	// advance clock far beyond maxTelemetryAge WITHOUT polling => telemetry stale
	clk.advance(60 * time.Second)
	r := sup.Readiness(clk.now())
	if r.Ready {
		t.Fatalf("must not be ready when telemetry stale, reasons=%v", r.Reasons)
	}
	foundStale := false
	for _, d := range r.Details {
		for _, rr := range d.Reasons {
			if rr == "TELEMETRY_STALE" || rr == "COLLECTOR_STALLED" {
				foundStale = true
			}
		}
	}
	if !foundStale {
		t.Fatalf("expected staleness reason, got %v", r.Details)
	}
}

func TestSupervisor_NotReadyWhenOffline(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second)
	mock.setHardFail("0")
	poll(sup, clk, 1, 2*time.Second)
	if sup.HostStatus().Devices[0].LifecycleState != core.StateOffline {
		t.Fatalf("expected OFFLINE, got %s", sup.HostStatus().Devices[0].LifecycleState)
	}
	if sup.Readiness(clk.now()).Ready {
		t.Fatal("must not be ready when OFFLINE")
	}
}

func TestSupervisor_SoftFailureDoesNotImmediatelyOffline(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 3, 2*time.Second)
	mock.setSoftFail("0")
	poll(sup, clk, 1, 2*time.Second)
	st := sup.HostStatus().Devices[0].LifecycleState
	if st == core.StateOffline {
		t.Fatalf("one soft failure must NOT be OFFLINE, got %s", st)
	}
	if st != core.StateDegraded {
		t.Fatalf("one soft failure should be DEGRADED, got %s", st)
	}
}

func TestSupervisor_HardFailureImmediateOffline(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 3, 2*time.Second)
	mock.setHardFail("0")
	poll(sup, clk, 1, 2*time.Second)
	d := sup.HostStatus().Devices[0]
	if d.LifecycleState != core.StateOffline {
		t.Fatalf("hard failure must be OFFLINE, got %s", d.LifecycleState)
	}
	if !d.Lifecycle.HardOffline {
		t.Fatal("must be flagged hard_offline")
	}
}

func TestSupervisor_WorkerStartAndDisappearEvents(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second) // baseline procs=0
	mock.setProcs("0", 1)
	mock.setMemUsed("0", 30e9)
	poll(sup, clk, 1, 2*time.Second) // worker appears
	mock.setProcs("0", 0)
	mock.setMemUsed("0", 1e9)
	poll(sup, clk, 1, 2*time.Second) // worker disappears
	events := sup.Events()
	var started, disappeared bool
	var crashClaimed bool
	for _, e := range events {
		switch e.Kind {
		case core.EventWorkerStarted:
			started = true
		case core.EventWorkerDisappeared:
			disappeared = true
			if e.TerminationCause != core.CauseUnknown {
				t.Fatalf("disappearance cause must be unknown, got %q", e.TerminationCause)
			}
			if e.GroundTruthSource != "" {
				t.Fatalf("no ground truth source expected, got %q", e.GroundTruthSource)
			}
		case core.EventWorkerCrashConfirmed:
			crashClaimed = true
		}
	}
	if !started {
		t.Fatal("expected WORKER_STARTED event")
	}
	if !disappeared {
		t.Fatal("expected WORKER_DISAPPEARED event")
	}
	if crashClaimed {
		t.Fatal("must NOT emit WORKER_CRASH_CONFIRMED from count delta alone")
	}
}

func TestSupervisor_BusyOnHighUtil(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 3, 2*time.Second)
	mock.setUtil("0", 95)
	poll(sup, clk, 3, 2*time.Second) // needs confirmation
	if st := sup.HostStatus().Devices[0].LifecycleState; st != core.StateBusy {
		t.Fatalf("sustained high util want BUSY, got %s", st)
	}
}

func TestSupervisor_RecoveryThroughRecovering(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 3, 2*time.Second)
	mock.setHardFail("0")
	poll(sup, clk, 1, 2*time.Second)
	if sup.HostStatus().Devices[0].LifecycleState != core.StateOffline {
		t.Fatal("setup: want OFFLINE")
	}
	mock.setHealthy("0")
	poll(sup, clk, 1, 2*time.Second)
	st := sup.HostStatus().Devices[0].LifecycleState
	if st != core.StateRecovering {
		t.Fatalf("first good probe after OFFLINE must be RECOVERING, got %s", st)
	}
	// sustained recovery
	poll(sup, clk, 6, 2*time.Second)
	st = sup.HostStatus().Devices[0].LifecycleState
	if st != core.StateReady {
		t.Fatalf("want READY after sustained recovery, got %s", st)
	}
	// rejoin accounting
	if sup.HostStatus().Devices[0].Reliability.RejoinCount < 1 {
		t.Fatal("expected rejoin count >= 1")
	}
}

func TestSupervisor_DisconnectRejoinEvents(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 3, 2*time.Second)
	mock.setHardFail("0")
	poll(sup, clk, 1, 2*time.Second)
	mock.setHealthy("0")
	poll(sup, clk, 2, 2*time.Second)
	var disc, rejoin bool
	for _, e := range sup.Events() {
		if e.Kind == core.EventDisconnect {
			disc = true
		}
		if e.Kind == core.EventRejoin {
			rejoin = true
		}
	}
	if !disc || !rejoin {
		t.Fatalf("expected disconnect+rejoin events, got disc=%v rejoin=%v", disc, rejoin)
	}
}

func TestSupervisor_DrainEventRecorded(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second)
	found, changed := sup.SetDraining("0", true, "test")
	if !found || !changed {
		t.Fatalf("drain set should succeed and change, got found=%v changed=%v", found, changed)
	}
	// idempotent
	_, changed2 := sup.SetDraining("0", true, "test")
	if changed2 {
		t.Fatal("repeated drain=true must be idempotent (changed=false)")
	}
	poll(sup, clk, 2, 2*time.Second)
	if st := sup.HostStatus().Devices[0].LifecycleState; st != core.StateDraining {
		t.Fatalf("want DRAINING after drain, got %s", st)
	}
}

func TestSupervisor_StaleClockSkewSafe(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 3, 2*time.Second)
	// simulate wall clock jumping backwards but monotonic still advancing
	clk.mu.Lock()
	clk.t = clk.t.Add(-time.Hour)
	clk.mu.Unlock()
	clk.advance(2 * time.Second) // mono advances, wall now = -1h+2s
	sup.PollOnce()
	// should not panic; availability should still be sane
	rel := sup.HostStatus().Devices[0].Reliability
	if rel.RecentAvailability < 0 || rel.RecentAvailability > 1 {
		t.Fatalf("availability out of range under clock skew: %v", rel.RecentAvailability)
	}
}
