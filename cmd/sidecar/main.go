// Command sidecar runs the cross-vendor GPU host sidecar daemon.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/adapters"
	"github.com/lokic233/gpu-sidecar/internal/api"
	"github.com/lokic233/gpu-sidecar/internal/cache"
	"github.com/lokic233/gpu-sidecar/internal/config"
	"github.com/lokic233/gpu-sidecar/internal/dataplane"
	"github.com/lokic233/gpu-sidecar/internal/engine"
	"github.com/lokic233/gpu-sidecar/internal/runtime/vllm"
	"github.com/lokic233/gpu-sidecar/internal/trajectory"
)

const Version = "0.1.0"

func main() {
	cfg := config.Default()
	var devicesCSV, vendor string
	var maxTelAge time.Duration
	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "listen address (default 127.0.0.1:9095 loopback-only; override for trusted mesh — see artifacts/api_security_notes.md)")
	flag.DurationVar(&cfg.PollInterval, "poll", cfg.PollInterval, "poll interval")
	flag.DurationVar(&cfg.ProbeTimeout, "probe-timeout", cfg.ProbeTimeout, "per-command timeout")
	flag.StringVar(&devicesCSV, "devices", "", "comma-separated device ids to monitor (default all)")
	flag.StringVar(&vendor, "vendor", "auto", "vendor: auto|nvidia|amd|generic")
	flag.BoolVar(&cfg.AccessProbeEach, "access-probe", cfg.AccessProbeEach, "active access probe each cycle")
	flag.DurationVar(&maxTelAge, "max-telemetry-age", 15*time.Second, "readiness: max age of a successful sample before /readyz fails")
	// --- data plane (Phase 1-4): vLLM runtime + admission queue + proxy ---
	var enableDataPlane bool
	var vllmBaseURL, backendID, dpDeviceID, collectorURL string
	var maxQueued, maxInflight int
	var queueTimeout time.Duration
	flag.BoolVar(&enableDataPlane, "data-plane", false, "enable local data plane (vLLM proxy + admission queue)")
	flag.StringVar(&vllmBaseURL, "vllm-url", "http://127.0.0.1:8000", "local vLLM base URL")
	flag.StringVar(&backendID, "backend-id", "", "backend id for this host (default hostname-gpuN)")
	flag.StringVar(&dpDeviceID, "dp-device", "", "device id this data plane serves (provenance)")
	flag.StringVar(&collectorURL, "collector-url", "", "trajectory collector URL (empty = disabled)")
	flag.IntVar(&maxQueued, "max-queued", 256, "max queued requests (admission queue)")
	flag.IntVar(&maxInflight, "max-inflight", 32, "max in-flight requests to vLLM")
	flag.DurationVar(&queueTimeout, "queue-timeout", 30*time.Second, "admission queue timeout")
	// --- cache-aware observation plane (all default OFF) ---
	var cacheObserver, cacheEventEndpoint string
	var cacheIndexMax int
	var cacheEntryTTL, cacheStaleAfter time.Duration
	var cacheExplicitHeader bool
	flag.StringVar(&cacheObserver, "cache-observer", "disabled", "cache observation provider: disabled|explicit|vllm-events")
	flag.StringVar(&cacheEventEndpoint, "cache-event-endpoint", "", "native KV-event transport endpoint (vllm-events mode; unwired on this stack)")
	flag.IntVar(&cacheIndexMax, "cache-index-max-entries", 100000, "max entries in the local prefix index")
	flag.DurationVar(&cacheEntryTTL, "cache-entry-ttl", 10*time.Minute, "TTL after which a prefix entry is treated as absent")
	flag.DurationVar(&cacheStaleAfter, "cache-stale-after", 30*time.Second, "if no cache event for this long, confidence->0 (fallback)")
	flag.BoolVar(&cacheExplicitHeader, "cache-explicit-header-enabled", false, "accept X-Cache-Prefix-Key/-Tokens (explicit experiment mode)")
	flag.Parse()


	host, _ := os.Hostname()
	bootID := readFirstLine("/proc/sys/kernel/random/boot_id")
	instanceID := host + "-" + time.Now().Format("20060102T150405")

	var adapter adapters.Adapter
	var desc string
	switch vendor {
	case "nvidia":
		adapter, desc = adapters.NewNVIDIA(), "nvidia (forced)"
	case "amd":
		adapter, desc = adapters.NewAMD(), "amd (forced)"
	case "generic":
		adapter, desc = adapters.NewGeneric(), "generic (forced)"
	default:
		adapter, desc = adapters.Detect()
	}
	// Fault injection is gated by GPU_SIDECAR_FAULT_FILE; safe no-op otherwise. Applies to all vendors.
	adapter = adapters.WrapFaultInject(adapter)
	log.Printf("sidecar %s starting on %s | adapter=%s | boot=%s", Version, host, desc, bootID)

	var devFilter []string
	if devicesCSV != "" {
		devFilter = strings.Split(devicesCSV, ",")
	}

	sup := engine.NewSupervisor(adapter, instanceID, host, bootID, Version,

		cfg.Lifecycle, cfg.Stability, cfg.WindowSeconds,
		cfg.ProbeRingCap, cfg.PointRingCap, cfg.EventRingCap, cfg.ProbeTimeout, cfg.AccessProbeEach)
	sup.SetMaxTelemetryAge(maxTelAge)
	sup.SetPollInterval(cfg.PollInterval)

	if err := sup.Init(devFilter); err != nil {
		log.Printf("WARN: discovery failed (%v) — serving degraded contract", err)
	}
	log.Printf("monitoring %d device(s)", sup.DeviceCount())

	// initial poll so /v1/status is populated immediately
	sup.PollOnce()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		t := time.NewTicker(cfg.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sup.PollOnce()
			}
		}
	}()

	apiServer := api.New(sup, Version)

	// --- Data plane wiring (Phase 1-4) ---
	var emitter *trajectory.Emitter
	if enableDataPlane {
		if backendID == "" {
			backendID = host + "-gpu" + dpDeviceID
		}
		// vLLM runtime adapter (separate metrics/health/proxy clients).
		vcfg := vllm.DefaultConfig()
		vcfg.BaseURL = vllmBaseURL
		vadapter := vllm.New(vcfg)
		vadapter.Start()
		defer vadapter.Stop()

		// trajectory emitter (non-blocking; collector configurable by URL).
		tcfg := trajectory.DefaultConfig()
		tcfg.Enabled = collectorURL != ""
		tcfg.CollectorURL = collectorURL
		tcfg.Source = "sidecar:" + host
		tcfg.LocalFallbackPath = "" // configurable; off by default
		emitter = trajectory.New(tcfg)
		emitter.Start()
		defer emitter.Stop()

		// bounded admission queue.
		qcfg := dataplane.DefaultQueueConfig()
		qcfg.MaxQueuedRequests = maxQueued
		qcfg.MaxInflightRequests = maxInflight
		qcfg.QueueTimeout = queueTimeout
		q := dataplane.NewQueue(qcfg, func(e dataplane.TransitionEvent) {
			emitter.Emit(trajectory.Event{
				Kind: "STATE_TRANSITION", Source: "sidecar:" + host, RequestID: e.RequestID,
				RouteID: e.RouteID, BackendID: e.BackendID, HostID: e.HostID, DeviceID: e.DeviceID,
				Wall: e.Wall, Fields: map[string]any{"from": e.From, "to": e.To, "reason": e.Reason},
			})
		})
		go q.Run(ctx)
		defer q.Close()

		// admission gate: refuse when lifecycle OFFLINE/DRAINING or vLLM unhealthy.
		gate := func() error {
			if !vadapter.Health() {
				return dataplane.ErrRuntimeUnhealthy
			}
			st, ok := sup.DeviceReadiness(dpDeviceID, time.Now())
			if ok {
				for _, rc := range st.Reasons {
					if rc == "LIFECYCLE_OFFLINE" {
						return dataplane.ErrBackendOffline
					}
				}
			}
			if sup.Draining(dpDeviceID) {
				return dataplane.ErrBackendDraining
			}
			return nil
		}

		pcfg := dataplane.DefaultProxyConfig()
		pcfg.HostID = host
		pcfg.BackendID = backendID
		pcfg.DeviceID = dpDeviceID
		pcfg.ExplicitHeaderEnabled = cacheExplicitHeader
		proxy := dataplane.NewProxy(pcfg, q, vadapter, gate, trajectory.SidecarSink{E: emitter})

		// optional token-level work accountant (additive to hard bounds; only when cache observation on)
		var workAcct *dataplane.WorkAccountant
		if cache.ParseMode(cacheObserver) != cache.ModeDisabled {
			workAcct = dataplane.NewWorkAccountant()
			proxy.SetWorkAccountant(workAcct)
		}

		// --- cache-observation plane (default disabled) ---
		cmode := cache.ParseMode(cacheObserver)
		var provider cache.Provider
		if cmode != cache.ModeDisabled {
			provider = cache.NewProvider(cache.ProviderConfig{
				Mode: cmode,
				Index: cache.IndexConfig{
					MaxEntries: cacheIndexMax, EntryTTL: cacheEntryTTL,
					StaleAfter: cacheStaleAfter, BlockSize: 16,
				},
				ExplicitHeaderEnabled: cacheExplicitHeader,
				EventEndpoint:         cacheEventEndpoint,
			})
			if err := provider.Start(ctx); err != nil {
				log.Printf("WARN: cache provider start failed (%v) — continuing without cache observation", err)
				provider = cache.NewDisabledProvider()
			} else {
				defer provider.Stop()
			}
			// explicit mode: let the proxy observe prefix locality on dispatch.
			if ep, ok := provider.(*cache.ExplicitProvider); ok {
				proxy.SetCacheObserver(ep)
			}
			// feed KV headroom + detect runtime restart for cache invalidation, off the hot path.
			go func() {
				t := time.NewTicker(1 * time.Second)
				defer t.Stop()
				lastUp := true
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						snap := vadapter.Snapshot()
						if snap.KVCacheUtil.Supported {
							provider.SetKVHeadroom(1-snap.KVCacheUtil.Value, true)
						} else {
							provider.SetKVHeadroom(0, false)
						}
						up := vadapter.Health()
						if up && !lastUp {
							provider.OnRuntimeRestart() // runtime came back -> KV cache was lost
						}
						lastUp = up
					}
				}
			}()
			dirMax := cacheIndexMax
			if dirMax > 4096 {
				dirMax = 4096 // bound the published directory
			}
			apiServer.AttachCacheObserver(func() any { return cache.BuildPublicSnapshot(provider, dirMax) })
			log.Printf("cache observation ENABLED: mode=%s explicit_header=%v index_max=%d ttl=%s stale_after=%s",
				cmode, cacheExplicitHeader, cacheIndexMax, cacheEntryTTL, cacheStaleAfter)
		}

		apiServer.AttachDataPlane(proxy.ChatCompletions,
			func() any { return vadapter.Snapshot() },
			func() any {
				qs := q.Snapshot()
				if workAcct == nil {
					return qs
				}
				// combine queue metrics + token-level work accounting into one /v1/queue body
				return map[string]any{
					"queued_requests": qs.Queued, "inflight_requests": qs.Inflight,
					"max_queued_requests": qs.MaxQueued, "max_inflight_requests": qs.MaxInflight,
					"oldest_queued_age_ms": qs.OldestAgeMs, "arrival_rate_per_s": qs.ArrivalRate,
					"dispatch_rate_per_s": qs.DispatchRate, "completion_rate_per_s": qs.CompletionRate,
					"arrivals_total": qs.Arrivals, "dispatched_total": qs.Dispatched,
					"completed_total": qs.Completed, "rejected_total": qs.Rejected,
					"queue_timeout_total": qs.TimedOut, "cancelled_total": qs.Cancelled,
					"queue_wait_p50_ms": qs.WaitP50Ms, "queue_wait_p95_ms": qs.WaitP95Ms,
					"enabled": qs.Enabled, "work_accounting": workAcct.Snapshot(),
				}
			})

		log.Printf("data plane ENABLED: vllm=%s backend=%s device=%s queue(max_q=%d,max_inflight=%d) collector=%q",
			vllmBaseURL, backendID, dpDeviceID, maxQueued, maxInflight, collectorURL)
	}

	// NOTE: WriteTimeout MUST be 0 (no deadline) so SSE streaming responses are not cut off.
	// Read timeout is bounded; idle/read-header timeouts protect against slow-loris.
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	if !config.IsLoopback(cfg.ListenAddr) {
		log.Printf("WARNING: binding %s is NON-loopback. The API has an UNauthenticated mutation "+
			"endpoint (/v1/drain) with no TLS/authz. Only expose on a trusted mesh interface. "+
			"See artifacts/api_security_notes.md", cfg.ListenAddr)
	}
	go func() {
		log.Printf("HTTP listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

func readFirstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
