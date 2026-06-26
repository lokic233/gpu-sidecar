# Implementation Report — Round-5 Correctness Hardening

Branch `navi/cache-hardening` (base `7034f4b`). Goal: close the semantic gap between the synthetic
cache experiment and real KV-cache behavior; make every cache-aware routing claim survive a hostile
systems reviewer. NO redesign, NO new feature layer, NO PPO. Incremental, behind existing flags,
cache OFF by default.

## Files changed (34 files, +2653/-540)
Code:
- internal/cache/residency.go (NEW): per-prefix ABSENT/WARMING/READY state machine.
- internal/cache/explicit_provider.go: rewritten to use residency (BeginWarm/MarkReady/AbortWarm/
  Reset/Lookup) — replaces binary Observe().
- internal/cache/provider.go: + ResidencyObserver interface.
- internal/cache/index.go: per-entry seq ordering; unresolved-gap trust state; stale-once counter;
  directory suppressed on gap.
- internal/cache/model.go: + residency/sequence-health snapshot fields.
- internal/dataplane/work.go: rewritten — Reserve@admission / Activate@dispatch / Release-once;
  queued/active/outstanding counters via per-ticket Reservation.
- internal/dataplane/proxy.go: residency lifecycle + reservation wired through ChatCompletions and
  ALL relay terminal paths via resolveTerminal(ready); reserve sized from local residency state.
- internal/dataplane/queue.go: Ticket carries reservation + prefix + warmBegun + resolved.
- internal/router/registry.go: single atomic BackendSnapshot{Generation,Backends,CacheDirectory};
  removed separate cacheDir/lock; + RuntimeInstanceID/RuntimeEndpointID; serviceRate kept as telemetry.
- internal/router/policy_cache_aware.go: locality from snapshot (not locator); BackendProfile split
  from live aggregate throughput; profile_fallback recorded; affinity policy reads snapshot.
- internal/router/policy.go: PolicyByNameWithProfiles.
- internal/router/gateway.go: shared scorer (profiles); snapshot_generation in 3 events.
- internal/runtime/{model.go,vllm/metrics.go}: + RuntimeInstanceID (process_start_time); endpoint id
  injected in sidecar /v1/runtime.
- cmd/router/main.go: --profiles flag. cmd/sidecar/main.go: runtime_endpoint_id + vllm_base_url in
  /v1/runtime.

Tests (NEW/rewritten): residency_test.go, snapshot_test.go, index_test.go (+ordering/trust),
provider_test.go (residency), policy_cache_aware_test.go (profiles/feedback), cache_proxy_test.go
(residency+work invariants).

Experiments/docs: experiments/cache_compare_equal.py (independent-replica guard), cache_phase_shift.py;
artifacts/cache_aware_sidecar_hardening/* (this dir).

## Bugs fixed
1. False-positive cache hit: prefix marked cached at dispatch even if the request failed/cancelled
   before first token. -> residency state machine; WARMING never a hit.
2. Work accounting reserved at dispatch (queued work uncounted) and recomputed independently. ->
   reserve@admission, ticket-carried, release-once; counters match documented semantics.
3. Cross-generation read: policy mixed snapshot backend-state with the LIVE cache directory. -> one
   atomic snapshot; locality resolved from the same generation.
4. Positive-feedback herding risk: aggregate Δtokens/Δt used as per-request decode speed. -> static
   BackendProfile; aggregate throughput is telemetry only.
5. Old event could override newer per-entry state (old remove deletes newer store / old store
   resurrects newer remove). -> per-entry seq ordering.
6. Cumulative gap counter used as a permanent confidence penalty. -> unresolved-gap trust state
   (conf 0 + no directory), restored only by reset/all-clear.
7. stale_invalidations_total incremented on every poll while stale. -> once per fresh->stale.
8. Equal-capability experiment ran two sidecars over ONE vLLM. -> independent replicas + HARD-STOP guard.

## Claims weakened / corrected
- "cache-aware balances / affinity herds" — previously shown only on the INVALID shared-runtime
  experiment; now demonstrated on TWO genuinely independent replicas (results.md §2-3) and labeled.
- Throughput parity caveat added honestly (tiny model doesn't saturate; assignment is the signal).
- Native matching remains "unsupported" — no overclaim. Preferred wording adopted: "Cache-aware
  routing contract + explicit-prefix controlled experiment validated; native KV event ingestion
  validated; native per-request KV-block matching remains unsupported."

## Exact test results
`go vet ./...` clean. `go test -race ./...` -> all 12 packages ok, race-clean. 207 test funcs.

## Independent replica evidence
independent_replica_proof.md: two vLLM 0.23.0 on H100 GPU6/GPU7, distinct runtime_endpoint_ids,
traffic-isolation counter test (A 36->221, B 0->0). Guard accepts these, rejects two-over-one.

## Real H100/MI350X runtime inventory
- H100 devgpu014: vLLM 0.23.0, GPU6 :8006 + GPU7 :8007 (equal-capability); GPU? :8000 (legacy).
- MI350X devgpu499: real ROCm vLLM 0.21.1, :8001 (heterogeneous run). NOT the mini HF server.

## Synthetic vs native cache semantics
Routing uses SYNTHETIC explicit-prefix locality (each key <-> a real identical prompt prefix); real
prefix-cache reuse confirmed separately via per-replica prefix_cache_hits delta. Native KV events are
ingested (metadata only) but per-request matching is unsupported. See remaining_blockers.md.

## Benchmark commands
```
go test -race ./... ; go vet ./...
artifacts/cache_aware_sidecar_hardening/launch_two_replicas.sh        # 2 independent H100 replicas
REQS=120 CONCS=1,4,8,16,32 python3 experiments/cache_compare_equal.py # guard-gated comparison
N=120 CONC=12 python3 experiments/cache_phase_shift.py                # phase-shift
# heterogeneous: router --profiles + real vLLM on H100 and MI350X (see experiment_protocol.md §C)
```

## Raw artifact locations
artifacts/cache_aware_sidecar_hardening/: pre_change_audit.md, correctness_design.md,
independent_replica_proof.md, {cache_residency,work_accounting,atomic_snapshot,service_profile}_contract.md,
experiment_protocol.md, results.md, implementation_report.md, remaining_blockers.md,
results_equal/{comparison,phase_shift}.json + run.log, results_hetero/hetero.json,
launch_two_replicas.sh, replica_logs/.

## Remaining blocker for native request-level matching
See remaining_blockers.md §1: needs version-specific block hashing + raw token IDs + extra-key
handling. Kept metadata-only; match_supported=false; no raw token storage.

## Recommended state/base-score interface for Liangqi's PPO
See remaining_blockers.md "Recommended interface": per-candidate state from one snapshot generation
incl. residency state (ABSENT/WARMING/READY), matched tokens only when READY, profile + fallback flag,
queued/active prefill+decode work, and the analytical breakdown. final_cost = analytical + residual.
