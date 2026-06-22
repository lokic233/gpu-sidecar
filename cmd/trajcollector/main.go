// Command trajcollector is the Response/Trajectory Collector. It asynchronously RECEIVES and
// PERSISTS metadata/outcome events from the Router and Sidecars. It is NOT a response proxy.
// Its failure/slowness must not block routing/queueing/inference/streaming (the emitters are
// non-blocking and bounded; this server just needs to accept POSTs and append).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var listen, outPath string
	flag.StringVar(&listen, "listen", "127.0.0.1:9100", "listen address (configurable; loopback default)")
	flag.StringVar(&outPath, "out", "artifacts/e2e_vllm_flow/router/joined_trajectories/events.jsonl", "append-only JSONL output path")
	flag.Parse()

	if err := os.MkdirAll(dirOf(outPath), 0755); err != nil {
		log.Printf("WARN: mkdir: %v", err)
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("open out: %v", err)
	}
	defer f.Close()

	var mu sync.Mutex
	var received atomic.Uint64

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
		var payload struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, ev := range payload.Events {
			f.Write(ev)
			f.Write([]byte("\n"))
		}
		mu.Unlock()
		received.Add(uint64(len(payload.Events)))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"alive","received":%d}`, received.Load())
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"received": received.Load(), "out": outPath})
	})

	srv := &http.Server{Addr: listen, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	log.Printf("trajectory collector listening on %s -> %s", listen, outPath)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("collector: %v", err)
	}
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
