#!/bin/bash
# Auto-restart wrapper for the cache-aware sidecar on the contended MI350X box (external SIGABRTs).
cd "$(dirname "$0")/.."
LOG=artifacts/cache_aware_sidecar/e2e/mi350x/sidecar_watch.log
while true; do
  echo "$(date) starting cache-aware sidecar" >> "$LOG"
  ./bin/sidecar -listen "[::]:19097" -devices 2 -poll 2s \
    -data-plane -vllm-url http://127.0.0.1:8000 -backend-id mi350x-gpu2 -dp-device 2 \
    -max-queued 256 -max-inflight 32 -queue-timeout 30s \
    -collector-url "http://[2401:db00:33c:2c1c:face:0:266:0]:29110/v1/events" \
    -cache-observer explicit -cache-explicit-header-enabled -cache-stale-after 30s \
    >> "$LOG" 2>&1
  echo "$(date) sidecar exited code=$? — restarting in 2s" >> "$LOG"
  sleep 2
done
