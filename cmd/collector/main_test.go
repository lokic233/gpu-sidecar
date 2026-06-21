package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

func mkStatus(host string, vendor core.Vendor, devs int, state core.LifecycleState, ts time.Time) core.HostStatus {
	hs := core.HostStatus{Hostname: host, Vendor: vendor, Timestamp: ts}
	for i := 0; i < devs; i++ {
		id := core.Identity{DeviceID: itoa(i), BackendID: host + "-gpu" + itoa(i), Vendor: vendor}
		hs.Devices = append(hs.Devices, core.DeviceStatus{
			Identity: id, LifecycleState: state,
			Health: core.Health{GPUVisible: true, Timestamp: ts, MemFreeBytes: core.Sup(uint64(90e9))},
			Stability: core.StabilityScore{Score: 0.95},
			Capacity:  core.CapacityHint{HostCapacityHint: 0.8, CapacitySemantics: "heuristic_host_derived"},
		})
	}
	return hs
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestCollector_HealthyBackend(t *testing.T) {
	hs := mkStatus("h100", core.VendorNVIDIA, 2, core.StateReady, time.Now())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hs)
	}))
	defer srv.Close()
	views := pollOne(&http.Client{Timeout: 2 * time.Second}, target{name: "h100", url: srv.URL})
	if len(views) != 2 {
		t.Fatalf("want 2 views got %d", len(views))
	}
	for _, v := range views {
		if !v.Reachable || v.Stale {
			t.Fatalf("healthy backend should be reachable and fresh: %+v", v)
		}
	}
}

func TestCollector_UnreachableBackend(t *testing.T) {
	views := pollOne(&http.Client{Timeout: 500 * time.Millisecond}, target{name: "dead", url: "http://127.0.0.1:1"})
	if len(views) != 1 || views[0].Reachable {
		t.Fatalf("unreachable backend must be marked unreachable: %+v", views)
	}
	if views[0].Error == "" {
		t.Fatal("unreachable backend must carry an error")
	}
}

func TestCollector_StaleBackend(t *testing.T) {
	// timestamp 30s in the past => stale
	hs := mkStatus("h100", core.VendorNVIDIA, 1, core.StateReady, time.Now().Add(-30*time.Second))
	views := viewsFromStatus("h100", &hs, time.Now())
	if !views[0].Stale {
		t.Fatalf("backend with 30s-old heartbeat must be stale, age=%dms", views[0].LastHeartbeatAgeMs)
	}
}

func TestCollector_FreshBackendNotStale(t *testing.T) {
	hs := mkStatus("h100", core.VendorNVIDIA, 1, core.StateReady, time.Now())
	views := viewsFromStatus("h100", &hs, time.Now())
	if views[0].Stale {
		t.Fatal("fresh backend must not be stale")
	}
}

func TestCollector_MalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not valid json"))
	}))
	defer srv.Close()
	views := pollOne(&http.Client{Timeout: 2 * time.Second}, target{name: "bad", url: srv.URL})
	if len(views) != 1 || views[0].Reachable {
		t.Fatalf("malformed response must be unreachable: %+v", views)
	}
}

func TestCollector_SlowEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		json.NewEncoder(w).Encode(mkStatus("slow", core.VendorAMD, 1, core.StateReady, time.Now()))
	}))
	defer srv.Close()
	start := time.Now()
	views := pollOne(&http.Client{Timeout: 300 * time.Millisecond}, target{name: "slow", url: srv.URL})
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("slow endpoint must be cut off by client timeout")
	}
	if views[0].Reachable {
		t.Fatal("timed-out backend must be unreachable")
	}
}

func TestCollector_RecoveringFlag(t *testing.T) {
	hs := mkStatus("h100", core.VendorNVIDIA, 1, core.StateRecovering, time.Now())
	views := viewsFromStatus("h100", &hs, time.Now())
	if !views[0].Recovering {
		t.Fatal("RECOVERING lifecycle must set recovering=true")
	}
}

func TestCollector_OfflineBackend(t *testing.T) {
	hs := mkStatus("h100", core.VendorNVIDIA, 1, core.StateOffline, time.Now())
	views := viewsFromStatus("h100", &hs, time.Now())
	if views[0].LifecycleState != core.StateOffline {
		t.Fatal("offline state must propagate")
	}
}

func TestCollector_MixedVendors(t *testing.T) {
	nv := mkStatus("h100", core.VendorNVIDIA, 2, core.StateReady, time.Now())
	amd := mkStatus("mi350x", core.VendorAMD, 3, core.StateReady, time.Now())
	v1 := viewsFromStatus("h100", &nv, time.Now())
	v2 := viewsFromStatus("mi350x", &amd, time.Now())
	all := append(v1, v2...)
	var nNV, nAMD int
	for _, v := range all {
		switch v.Vendor {
		case core.VendorNVIDIA:
			nNV++
		case core.VendorAMD:
			nAMD++
		}
	}
	if nNV != 2 || nAMD != 3 {
		t.Fatalf("mixed vendor counts wrong: nv=%d amd=%d", nNV, nAMD)
	}
}

func TestCollector_DuplicateBackendIDs(t *testing.T) {
	// two hosts reporting same backend_id should both appear (collector does not dedup silently)
	hs := mkStatus("dup", core.VendorNVIDIA, 1, core.StateReady, time.Now())
	v1 := viewsFromStatus("dup", &hs, time.Now())
	v2 := viewsFromStatus("dup", &hs, time.Now())
	if v1[0].BackendID != v2[0].BackendID {
		t.Fatal("expected identical backend ids in this scenario")
	}
	all := append(v1, v2...)
	if len(all) != 2 {
		t.Fatalf("both duplicate-id entries must be preserved, got %d", len(all))
	}
}
