// Command router runs the experimental Global Router Gateway. It reads an in-memory backend
// snapshot (materialized off the hot path), selects a backend with a deterministic policy, forwards
// to the selected host's sidecar, relays the response (JSON or SSE) to the client, applies bounded
// pre-first-token retry, propagates cancellation, and emits async trajectory events.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lokic233/gpu-sidecar/internal/router"
	"github.com/lokic233/gpu-sidecar/internal/trajectory"
)

func main() {
	var listen, backendsJSON, policyName, collectorURL, profilesJSON string
	var snapInterval time.Duration
	var maxRetries int
	flag.StringVar(&listen, "listen", "127.0.0.1:9090", "client-facing listen address")
	flag.StringVar(&backendsJSON, "backends", "", "JSON array of backends [{id,vendor,sidecar_url,snapshot_url}]")
	flag.StringVar(&policyName, "policy", "least_queued", "round_robin|least_queued|least_runtime_waiting|health_gated_least_pressure|cache_aware_estimated_finish|cache_affinity_only")
	flag.StringVar(&collectorURL, "collector-url", "", "trajectory collector URL (empty = disabled)")
	flag.StringVar(&profilesJSON, "profiles", "", `optional per-backend static service profiles, JSON map id->{"decode_ms_per_token":..,"prefill_ms_per_token":..}; absent => global fallback`)
	flag.DurationVar(&snapInterval, "snapshot-interval", 500*time.Millisecond, "backend snapshot refresh cadence")
	flag.IntVar(&maxRetries, "max-retries", 1, "max cross-backend pre-first-token retries")
	flag.Parse()

	var backends []router.Backend
	if backendsJSON != "" {
		if err := json.Unmarshal([]byte(backendsJSON), &backends); err != nil {
			log.Fatalf("parse -backends: %v", err)
		}
	}
	if len(backends) == 0 {
		log.Fatalf("no backends configured (use -backends)")
	}
	var profiles map[string]router.BackendProfile
	if profilesJSON != "" {
		if err := json.Unmarshal([]byte(profilesJSON), &profiles); err != nil {
			log.Fatalf("parse -profiles: %v", err)
		}
	}

	reg := router.NewRegistry(backends, snapInterval)
	reg.Start()
	defer reg.Stop()

	tcfg := trajectory.DefaultConfig()
	tcfg.Enabled = collectorURL != ""
	tcfg.CollectorURL = collectorURL
	tcfg.Source = "router"
	emitter := trajectory.New(tcfg)
	emitter.Start()
	defer emitter.Stop()

	gw := router.NewGatewayWithProfiles(reg, router.PolicyByNameWithProfiles(policyName, profiles), emitter, maxRetries, profiles)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", gw.ChatCompletions)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200); w.Write([]byte(`{"status":"alive"}`))
	})
	mux.HandleFunc("/v1/backends", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reg.Snapshot())
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// WriteTimeout=0: streaming responses must not be cut off.
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		log.Printf("router listening on %s | policy=%s | backends=%d | collector=%q", listen, policyName, len(backends), collectorURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("router http: %v", err)
		}
	}()
	<-ctx.Done()
	log.Printf("router shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	_ = os.Stdout
}
