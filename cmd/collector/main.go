// Command collector polls multiple GPU sidecars over the mesh and produces one normalized
// backend view. It is NOT a scheduler: it does not decide which request goes to which GPU.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
)

// BackendView is the router-facing per-backend summary. Field semantics documented in README.
type BackendView struct {
	BackendID         string              `json:"backend_id"`
	Hostname          string              `json:"hostname"`
	Vendor            core.Vendor         `json:"vendor"`
	DeviceID          string              `json:"device_id"`
	LifecycleState    core.LifecycleState `json:"lifecycle_state"`
	StabilityScore    float64             `json:"stability_score"`
	HostCapacityHint  float64             `json:"host_capacity_hint"`
	CapacitySemantics string              `json:"capacity_semantics"`
	LastHeartbeatAgeMs int64              `json:"last_heartbeat_age_ms"`
	RecentFailureCount int                `json:"recent_failure_count"`
	Recovering        bool                `json:"recovering"`
	Reachable         bool                `json:"reachable"`
	Stale             bool                `json:"stale"` // heartbeat age exceeded staleness threshold
	MemFreeBytes      uint64              `json:"mem_free_bytes"`
	Error             string              `json:"error,omitempty"`
}

// staleThresholdMs: a backend whose newest heartbeat is older than this is flagged stale.
const staleThresholdMs = 10000

type target struct {
	name string
	url  string
}

func main() {
	var targetsCSV, format string
	var interval time.Duration
	var once bool
	var timeout time.Duration
	flag.StringVar(&targetsCSV, "sidecars", "", "comma list name=url, e.g. h100=http://[::1]:19095,mi350x=http://[addr]:19095")
	flag.StringVar(&format, "format", "table", "table|json")
	flag.DurationVar(&interval, "interval", 3*time.Second, "poll interval")
	flag.BoolVar(&once, "once", false, "poll once and exit")
	flag.DurationVar(&timeout, "timeout", 4*time.Second, "per-sidecar HTTP timeout")
	flag.Parse()

	if targetsCSV == "" {
		fmt.Fprintln(os.Stderr, "need -sidecars name=url,...")
		os.Exit(2)
	}
	var targets []target
	for _, pair := range strings.Split(targetsCSV, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		targets = append(targets, target{name: kv[0], url: strings.TrimRight(kv[1], "/")})
	}

	client := &http.Client{Timeout: timeout}
	poll := func() []BackendView {
		var mu sync.Mutex
		var views []BackendView
		var wg sync.WaitGroup
		for _, t := range targets {
			wg.Add(1)
			go func(t target) {
				defer wg.Done()
				vs := pollOne(client, t)
				mu.Lock()
				views = append(views, vs...)
				mu.Unlock()
			}(t)
		}
		wg.Wait()
		sort.Slice(views, func(i, j int) bool {
			if views[i].Hostname != views[j].Hostname {
				return views[i].Hostname < views[j].Hostname
			}
			return views[i].DeviceID < views[j].DeviceID
		})
		return views
	}

	render := func(views []BackendView) {
		if format == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(views)
			return
		}
		printTable(views)
	}

	if once {
		render(poll())
		return
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	t := time.NewTicker(interval)
	defer t.Stop()
	render(poll())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fmt.Printf("\n--- %s ---\n", time.Now().Format(time.RFC3339))
			render(poll())
		}
	}
}

func pollOne(client *http.Client, t target) []BackendView {
	reach := func() (*core.HostStatus, error) {
		req, _ := http.NewRequest("GET", t.url+"/v1/status", nil)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var hs core.HostStatus
		if err := json.NewDecoder(resp.Body).Decode(&hs); err != nil {
			return nil, err
		}
		return &hs, nil
	}
	hs, err := reach()
	if err != nil {
		return []BackendView{{BackendID: t.name, Hostname: t.name, Reachable: false, Error: err.Error()}}
	}
	return viewsFromStatus(t.name, hs, time.Now())
}

// viewsFromStatus converts a HostStatus into BackendViews, flagging stale heartbeats.
// Separated for deterministic testing with an injectable 'now'.
func viewsFromStatus(name string, hs *core.HostStatus, now time.Time) []BackendView {
	var views []BackendView
	for _, d := range hs.Devices {
		age := now.Sub(d.Health.Timestamp).Milliseconds()
		if age < 0 {
			age = 0
		}
		var memFree uint64
		if d.Health.MemFreeBytes.Supported {
			memFree = d.Health.MemFreeBytes.Value
		}
		views = append(views, BackendView{
			BackendID:          d.Identity.BackendID,
			Hostname:           hs.Hostname,
			Vendor:             hs.Vendor,
			DeviceID:           d.Identity.DeviceID,
			LifecycleState:     d.LifecycleState,
			StabilityScore:     d.Stability.Score,
			HostCapacityHint:   d.Capacity.HostCapacityHint,
			CapacitySemantics:  d.Capacity.CapacitySemantics,
			LastHeartbeatAgeMs: age,
			RecentFailureCount: d.Reliability.ConsecutiveFailures,
			Recovering:         d.LifecycleState == core.StateRecovering,
			Reachable:          true,
			Stale:              age > staleThresholdMs,
			MemFreeBytes:       memFree,
		})
	}
	if len(views) == 0 {
		return []BackendView{{BackendID: name, Hostname: hs.Hostname, Reachable: true, Error: "no devices"}}
	}
	return views
}

func printTable(views []BackendView) {
	fmt.Printf("%-28s %-8s %-4s %-11s %7s %7s %9s %6s %6s\n",
		"BACKEND", "VENDOR", "DEV", "STATE", "STABIL", "CAPHINT", "HB_AGE_MS", "FAILS", "FREEGB")
	fmt.Println(strings.Repeat("-", 100))
	for _, v := range views {
		if !v.Reachable {
			fmt.Printf("%-28s %-8s %-4s %-11s   UNREACHABLE: %s\n", v.BackendID, v.Vendor, "-", "OFFLINE", v.Error)
			continue
		}
		fmt.Printf("%-28s %-8s %-4s %-11s %7.3f %7.3f %9d %6d %6.1f\n",
			truncate(v.BackendID, 28), v.Vendor, v.DeviceID, v.LifecycleState,
			v.StabilityScore, v.HostCapacityHint, v.LastHeartbeatAgeMs, v.RecentFailureCount,
			float64(v.MemFreeBytes)/1e9)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
