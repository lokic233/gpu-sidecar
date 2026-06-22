// Package api exposes the sidecar over HTTP: health, readiness, status, history, events, metrics.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/core"
	"github.com/lokic233/gpu-sidecar/internal/engine"
)

type Server struct {
	sup     *engine.Supervisor
	version string
	mux     *http.ServeMux

	// optional data plane (Phase 3+): nil for telemetry-only sidecars (backward compatible).
	chatHandler http.HandlerFunc
	runtimeSnap func() any
	queueSnap   func() any
}

func New(sup *engine.Supervisor, version string) *Server {
	s := &Server{sup: sup, version: version, mux: http.NewServeMux()}
	s.mux.HandleFunc("/healthz", s.healthz)
	s.mux.HandleFunc("/readyz", s.readyz)
	s.mux.HandleFunc("/v1/status", s.status)
	s.mux.HandleFunc("/v1/history", s.history)
	s.mux.HandleFunc("/v1/events", s.events)
	s.mux.HandleFunc("/v1/drain", s.drain)
	s.mux.HandleFunc("/v1/runtime", s.runtime)
	s.mux.HandleFunc("/v1/queue", s.queue)
	s.mux.HandleFunc("/v1/chat/completions", s.chat)
	s.mux.HandleFunc("/metrics", s.metrics)
	s.mux.HandleFunc("/", s.index)
	return s
}

// AttachDataPlane wires the local data-plane proxy + runtime/queue snapshot providers.
// Safe to skip for telemetry-only sidecars.
func (s *Server) AttachDataPlane(chat http.HandlerFunc, runtimeSnap, queueSnap func() any) {
	s.chatHandler = chat
	s.runtimeSnap = runtimeSnap
	s.queueSnap = queueSnap
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if s.chatHandler == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "data plane not enabled on this sidecar (telemetry-only)"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}
	s.chatHandler(w, r)
}

func (s *Server) runtime(w http.ResponseWriter, r *http.Request) {
	if s.runtimeSnap == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runtime_enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.runtimeSnap())
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	if s.queueSnap == nil {
		writeJSON(w, http.StatusOK, map[string]any{"queue_enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.queueSnap())
}


func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// /healthz: confirms the sidecar PROCESS is alive. Always 200 if we can serve.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "alive", "version": s.version, "time": time.Now()})
}

// /readyz: host-level control-plane readiness, OR per-device readiness with ?device=N.
//
// Host /readyz: 200 if the sidecar has collected, is not stalled, and can provide trustworthy
// status for at least one managed device (control-plane readiness — NOT proof every GPU can serve).
// Response exposes any_device_ready / all_devices_ready / ready_device_count / total_device_count.
//
// /readyz?device=N: 200 if that specific device satisfies readiness, 503 if not, 404 if unmanaged.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	if dev := r.URL.Query().Get("device"); dev != "" {
		dr, found := s.sup.DeviceReadiness(dev, now)
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "unmanaged device", "device": dev})
			return
		}
		code := http.StatusOK
		if !dr.Ready {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, dr)
		return
	}
	res := s.sup.Readiness(now)
	code := http.StatusOK
	if !res.ControlPlaneReady {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, res)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sup.HostStatus())
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	pts := s.sup.History()
	if dev := r.URL.Query().Get("device"); dev != "" {
		var f []core.HistoryPoint
		for _, p := range pts {
			if p.DeviceID == dev {
				f = append(f, p)
			}
		}
		pts = f
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(pts), "points": pts})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	evs := s.sup.Events()
	sort.Slice(evs, func(i, j int) bool { return evs[i].Timestamp.Before(evs[j].Timestamp) })
	writeJSON(w, http.StatusOK, map[string]any{"count": len(evs), "events": evs})
}

// /v1/drain : operator drain toggle. STATE-CHANGING — POST/PUT only, GET is rejected.
// Body or query: device=N, on=true|false. Idempotent. Records a lifecycle event.
func (s *Server) drain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "drain is state-changing; use POST or PUT, not " + r.Method})
		return
	}
	// Accept JSON body {"device":"N","on":true} or form/query params.
	var dev, onStr string
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var body struct {
			Device *string `json:"device"`
			On     *bool   `json:"on"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body: " + err.Error()})
			return
		}
		if body.Device == nil || body.On == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fields 'device' and 'on' are required"})
			return
		}
		dev = *body.Device
		onStr = strconv.FormatBool(*body.On)
	} else {
		dev = r.FormValue("device")
		onStr = r.FormValue("on")
	}
	if dev == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "field 'device' is required"})
		return
	}
	on, err := strconv.ParseBool(onStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "field 'on' must be a boolean (got " + strconv.Quote(onStr) + ")"})
		return
	}
	source := r.RemoteAddr
	found, changed := s.sup.SetDraining(dev, on, source)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown device", "device": dev})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": dev, "draining": on, "changed": changed})
}

// /metrics: Prometheus text exposition.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	hs := s.sup.HostStatus()
	var b strings.Builder
	help := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	lbl := func(d core.DeviceStatus) string {
		return fmt.Sprintf(`{host=%q,vendor=%q,device=%q,backend=%q}`,
			hs.Hostname, hs.Vendor, d.Identity.DeviceID, d.Identity.BackendID)
	}
	help("gpu_sidecar_up", "gauge", "1 if sidecar process is alive")
	fmt.Fprintf(&b, "gpu_sidecar_up{host=%q} 1\n", hs.Hostname)
	help("gpu_sidecar_uptime_seconds", "gauge", "sidecar uptime")
	fmt.Fprintf(&b, "gpu_sidecar_uptime_seconds{host=%q} %f\n", hs.Hostname, hs.UptimeSeconds)

	help("gpu_stability_score", "gauge", "normalized stability score [0,1]")
	help("gpu_host_capacity_hint", "gauge", "heuristic host-derived capacity hint [0,1] — NOT serving capacity")
	help("gpu_lifecycle_state", "gauge", "1 for the active lifecycle state label")
	help("gpu_utilization_pct", "gauge", "GPU utilization percent")
	help("gpu_mem_used_bytes", "gauge", "GPU memory used bytes")
	help("gpu_mem_free_bytes", "gauge", "GPU memory free bytes")
	help("gpu_temperature_celsius", "gauge", "GPU temperature C")
	help("gpu_power_watts", "gauge", "GPU power draw W")
	help("gpu_probe_latency_ms", "gauge", "telemetry probe latency ms")
	help("gpu_consecutive_probe_failures", "gauge", "consecutive probe failures")
	help("gpu_recent_availability", "gauge", "recent availability ratio")
	help("gpu_disconnect_count", "counter", "observed disconnects")
	help("gpu_rejoin_count", "counter", "observed rejoins")
	help("gpu_worker_starts_total", "counter", "observed worker starts")
	help("gpu_worker_stops_total", "counter", "observed worker stops")
	help("gpu_ecc_uncorrectable_total", "counter", "NVIDIA ECC uncorrectable (or AMD RAS)")

	states := []core.LifecycleState{core.StateUnknown, core.StateReady, core.StateBusy, core.StateDegraded, core.StateDraining, core.StateOffline, core.StateRecovering}
	for _, d := range hs.Devices {
		l := lbl(d)
		fmt.Fprintf(&b, "gpu_stability_score%s %f\n", l, d.Stability.Score)
		fmt.Fprintf(&b, "gpu_host_capacity_hint%s %f\n", l, d.Capacity.HostCapacityHint)
		for _, st := range states {
			v := 0
			if d.LifecycleState == st {
				v = 1
			}
			fmt.Fprintf(&b, "gpu_lifecycle_state{host=%q,vendor=%q,device=%q,state=%q} %d\n", hs.Hostname, hs.Vendor, d.Identity.DeviceID, st, v)
		}
		if d.Health.UtilizationGPU.Supported {
			fmt.Fprintf(&b, "gpu_utilization_pct%s %f\n", l, d.Health.UtilizationGPU.Value)
		}
		if d.Health.MemUsedBytes.Supported {
			fmt.Fprintf(&b, "gpu_mem_used_bytes%s %d\n", l, d.Health.MemUsedBytes.Value)
		}
		if d.Health.MemFreeBytes.Supported {
			fmt.Fprintf(&b, "gpu_mem_free_bytes%s %d\n", l, d.Health.MemFreeBytes.Value)
		}
		if d.Health.TemperatureC.Supported {
			fmt.Fprintf(&b, "gpu_temperature_celsius%s %f\n", l, d.Health.TemperatureC.Value)
		}
		if d.Health.PowerWatts.Supported {
			fmt.Fprintf(&b, "gpu_power_watts%s %f\n", l, d.Health.PowerWatts.Value)
		}
		fmt.Fprintf(&b, "gpu_probe_latency_ms%s %f\n", l, d.Health.ProbeLatencyMs)
		fmt.Fprintf(&b, "gpu_consecutive_probe_failures%s %d\n", l, d.Reliability.ConsecutiveFailures)
		fmt.Fprintf(&b, "gpu_recent_availability%s %f\n", l, d.Reliability.RecentAvailability)
		fmt.Fprintf(&b, "gpu_disconnect_count%s %d\n", l, d.Reliability.DisconnectCount)
		fmt.Fprintf(&b, "gpu_rejoin_count%s %d\n", l, d.Reliability.RejoinCount)
		fmt.Fprintf(&b, "gpu_worker_starts_total%s %d\n", l, d.Reliability.WorkerStarts)
		fmt.Fprintf(&b, "gpu_worker_stops_total%s %d\n", l, d.Reliability.WorkerStops)
		if d.Health.ECCUncorrectable.Supported {
			fmt.Fprintf(&b, "gpu_ecc_uncorrectable_total%s %d\n", l, d.Health.ECCUncorrectable.Value)
		} else if d.Health.RASUncorrectable.Supported {
			fmt.Fprintf(&b, "gpu_ecc_uncorrectable_total%s %d\n", l, d.Health.RASUncorrectable.Value)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "gpu-host-sidecar", "version": s.version,
		"endpoints": []string{"/healthz", "/readyz", "/v1/status", "/v1/history", "/v1/events", "/v1/drain", "/metrics"},
	})
}
