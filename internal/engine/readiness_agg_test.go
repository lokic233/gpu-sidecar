package engine

import (
	"testing"
	"time"
)

func TestReadinessAgg_FirstCollectionNotDone(t *testing.T) {
	mock := newMockAdapter("0", "1")
	sup, clk := newTestSup(t, mock, "0", "1")
	// no PollOnce
	r := sup.Readiness(clk.now())
	if r.ControlPlaneReady || r.AnyDeviceReady {
		t.Fatal("must not be ready before first collection")
	}
	if r.TotalDeviceCount != 2 {
		t.Fatalf("want total 2, got %d", r.TotalDeviceCount)
	}
}

func TestReadinessAgg_AllReady(t *testing.T) {
	mock := newMockAdapter("0", "1", "2")
	sup, clk := newTestSup(t, mock, "0", "1", "2")
	poll(sup, clk, 2, 2*time.Second)
	r := sup.Readiness(clk.now())
	if !r.AllDevicesReady || !r.AnyDeviceReady || !r.ControlPlaneReady {
		t.Fatalf("all should be ready: %+v", r)
	}
	if r.ReadyDeviceCount != 3 {
		t.Fatalf("want 3 ready, got %d", r.ReadyDeviceCount)
	}
}

func TestReadinessAgg_SomeReady(t *testing.T) {
	mock := newMockAdapter("0", "1", "2")
	sup, clk := newTestSup(t, mock, "0", "1", "2")
	poll(sup, clk, 2, 2*time.Second)
	mock.setHardFail("1")
	poll(sup, clk, 1, 2*time.Second)
	r := sup.Readiness(clk.now())
	if !r.AnyDeviceReady {
		t.Fatal("any should be ready (2/3)")
	}
	if r.AllDevicesReady {
		t.Fatal("all must be false")
	}
	if !r.ControlPlaneReady {
		t.Fatal("control plane ready with 2/3 ok")
	}
	if r.ReadyDeviceCount != 2 {
		t.Fatalf("want 2 ready, got %d", r.ReadyDeviceCount)
	}
}

func TestReadinessAgg_CollectorStalled(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second)
	if !sup.Readiness(clk.now()).ControlPlaneReady {
		t.Fatal("precondition ready")
	}
	// advance far without polling => stalled
	clk.advance(60 * time.Second)
	r := sup.Readiness(clk.now())
	if r.ControlPlaneReady {
		t.Fatal("stalled collector must not be control-plane ready")
	}
	found := false
	for _, rr := range r.Reasons {
		if rr == "COLLECTOR_STALLED" || rr == "TELEMETRY_STALE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want stall/stale reason, got %v", r.Reasons)
	}
}

func TestReadinessAgg_PerDeviceStale(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second)
	clk.advance(60 * time.Second)
	dr, found := sup.DeviceReadiness("0", clk.now())
	if !found {
		t.Fatal("device 0 should be found")
	}
	if dr.Ready {
		t.Fatal("stale device must not be ready")
	}
}

func TestReadinessAgg_PerDeviceUnmanaged(t *testing.T) {
	mock := newMockAdapter("0")
	sup, clk := newTestSup(t, mock, "0")
	poll(sup, clk, 2, 2*time.Second)
	if _, found := sup.DeviceReadiness("9", clk.now()); found {
		t.Fatal("unmanaged device must report found=false")
	}
}
