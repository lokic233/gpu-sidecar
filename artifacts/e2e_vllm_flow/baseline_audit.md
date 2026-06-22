# Baseline Audit (Phase 0)

## Baseline commit
`e6acf9c` — "docs(polish): final_polish artifacts ..." (round-3 final polish complete).

## Pre-change test results
`go test ./...` — all 7 packages PASS (collector, adapters, api, config, core, engine, exec).
112 tests, race-clean (from round 3).

## Existing structure inspected (preserved)
- `internal/core`: model, lifecycle (recovery latch), stability, history. PRESERVED.
- `internal/engine`: supervisor (telemetry poll loop, readiness, drain). EXTENDED (added Draining accessor).
- `internal/adapters`: nvidia/amd/generic/faultinject telemetry. PRESERVED.
- `internal/api`: HTTP server. EXTENDED (AttachDataPlane; added /v1/chat/completions, /v1/runtime,
  /v1/queue; FIXED WriteTimeout 15s->0 so SSE streaming isn't cut off — backward compatible).
- `internal/config`: loopback default. PRESERVED.
- `cmd/sidecar`: EXTENDED with -data-plane flags. `cmd/collector` (mesh aggregator) PRESERVED.
- Round-1/2/3 artifacts under `artifacts/`: PRESERVED.

## No pre-existing queue/router/proxy/vLLM code
There was no prior queue, router, proxy, or vLLM integration. This round adds:
- `internal/runtime` + `internal/runtime/vllm` (runtime adapter)
- `internal/dataplane` (admission queue + OpenAI proxy + SSE relay)
- `internal/trajectory` (async event emitter)
- `internal/router` (gateway + registry + policy)
- `cmd/router`, `cmd/trajcollector`

## Compatibility
No existing API fields removed. Telemetry-only sidecars (no -data-plane) behave exactly as before;
new endpoints return {"...enabled":false} / 501 when the data plane is not attached.
