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
	"github.com/lokic233/gpu-sidecar/internal/config"
	"github.com/lokic233/gpu-sidecar/internal/engine"
)

const Version = "0.1.0"

func main() {
	cfg := config.Default()
	var devicesCSV, vendor string
	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "listen address")
	flag.DurationVar(&cfg.PollInterval, "poll", cfg.PollInterval, "poll interval")
	flag.DurationVar(&cfg.ProbeTimeout, "probe-timeout", cfg.ProbeTimeout, "per-command timeout")
	flag.StringVar(&devicesCSV, "devices", "", "comma-separated device ids to monitor (default all)")
	flag.StringVar(&vendor, "vendor", "auto", "vendor: auto|nvidia|amd|generic")
	flag.BoolVar(&cfg.AccessProbeEach, "access-probe", cfg.AccessProbeEach, "active access probe each cycle")
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
		adapter = adapters.WrapFaultInject(adapter)
	}
	log.Printf("sidecar %s starting on %s | adapter=%s | boot=%s", Version, host, desc, bootID)

	var devFilter []string
	if devicesCSV != "" {
		devFilter = strings.Split(devicesCSV, ",")
	}

	sup := engine.NewSupervisor(adapter, instanceID, host, bootID, Version,
		cfg.Lifecycle, cfg.Stability, cfg.WindowSeconds,
		cfg.ProbeRingCap, cfg.PointRingCap, cfg.EventRingCap, cfg.ProbeTimeout, cfg.AccessProbeEach)

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

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: api.New(sup, Version).Handler(),
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second}
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
