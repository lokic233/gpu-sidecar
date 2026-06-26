# Correctness Design — Round-5 Hardening

How each P0/P1 correctness gap (from pre_change_audit.md) was closed, with the invariant it restores.
All changes are incremental and behind the existing feature flags; cache observation stays OFF by
default. See the per-area contracts for detail.

## P0 #2 — Valid two-replica experiment (independent runtimes)
- Problem: equal-capability comparison ran two sidecars over ONE vLLM (shared KV cache/scheduler).
- Fix: launch TWO independent vLLM processes on two GPUs (`launch_two_replicas.sh`), each its own
  sidecar; `cache_compare_equal.py` calls `assert_independent_replicas()` and HARD-STOPs if backends
  share a runtime identity. Identity = `runtime_endpoint_id` (hash of host+vllm-url+boot) surfaced by
  the sidecar `/v1/runtime`; `runtime_instance_id` (process_start_time) is a secondary signal.
- Proof: independent_replica_proof.md (distinct ids + traffic-isolation counter test). Guard verified
  to ACCEPT the two real replicas and REJECT two sidecars over one vLLM.

## P0 #3 — Residency state machine (not binary observe-at-dispatch)
- Problem: a prefix was marked cached at dispatch; a request that failed/cancelled before first token
  left a false-positive hit.
- Fix: `internal/cache/residency.go` ABSENT→WARMING→READY. `proxy.resolveTerminal(ready)` drives the
  transition exactly once on every terminal path: first token / 2xx → MarkReady; pre-first-token
  failure / non-2xx / cancel-before-ready / restart → AbortWarm. WARMING is never a hit and never in
  the directory. Concurrent warmers refcounted; stale MarkReady cannot resurrect (false_ready).
- Invariant restored: "a reusable cache hit reflects genuinely-resident KV", and "failure/cancel
  leaves no phantom locality".

## P0 #4 — Work accounting reserve-at-admission, ticket-carried, release-once
- Problem: reservations were booked at dispatch (queued work uncounted) and recomputed independently.
- Fix: `Reserve` at queue admission (queued bucket), `Activate` at dispatch (queued→active),
  `Release` once on every terminal path; the `*Reservation` is carried on the ticket. Counters split
  queued/active/outstanding (=queued+active); never negative; return to 0. READY→reserve uncached,
  else full prompt.
- Invariant restored: documented semantics ("reservations include queued and active work") now match
  the code.

## P0 #5 — One atomic routing snapshot (no cross-generation mix)
- Problem: backend state and cache directory were two separate stores; the policy read snapshot state
  but the LIVE directory.
- Fix: `BackendSnapshot{Generation, Backends, CacheDirectory}` published with ONE atomic swap; the
  policy resolves locality from `snap.LookupPrefixTokens` (same pointer). `snapshot_generation`
  emitted in BACKEND_SNAPSHOT_READ / CANDIDATE_STATE / ROUTE_DECIDED.
- Invariant restored: a routing decision sees a self-consistent (state, directory, epoch) tuple.
  Proven race-clean (snapshot_test.go).

## P1 #6 — Service profile decoupled from live aggregate throughput
- Problem: decode cost = 1000/aggregate_tokens_per_sec → busy backend looks faster → herding loop.
- Fix: `BackendProfile` (static, configured via `--profiles`) supplies decode/prefill ms/token; a
  global fallback is used + recorded when absent. Aggregate throughput is kept as TELEMETRY only.
  Live congestion (queue/inflight/runtime waiting/running) drives the queue term, so a growing
  backlog makes a backend LESS attractive.
- Invariant restored: capability ≠ congestion; no positive-feedback herding. Proven by
  TestCacheAware_BusyAggregateThroughputDoesNotAttract.

## P1 #7 — Event ordering + trust + stale semantics
- Per-entry sequence ordering: an old remove cannot delete a newer store; an old store cannot
  resurrect a newer remove (`ApplyStore`/`ApplyRemove` compare `seq` vs `entry.LastSeq`).
- Unresolved-gap TRUST state: a sequence gap sets `unresolvedGap` → confidence 0 + no matchable
  directory, until a verified Reset/AllBlocksCleared restores trust. NOT a cumulative soft penalty.
- Stale counter: `stale_invalidations_total` increments ONCE per fresh→stale transition (tracked via
  `wasStale`), not on every poll.
- Invariant restored: directory correctness under reordering/gaps; honest, non-monotonic-noise counters.

## P0 #8/§8 — Native matching stays honestly unsupported
- `vllm_events` remains metadata-only; `match_supported=false`; no matchable native directory; no raw
  token storage. The explicit experiment maps each prefix key to a REAL identical prompt prefix
  (experiment_protocol.md). See remaining_blockers.md.

## Preserved invariants (unchanged, re-verified)
Router owns selection; sidecar owns local admission/observation; vLLM owns KV; no hot-path scrape;
cache failure never blocks inference; sidecar queue ≠ vLLM queue; SSE/cancel/drain/retry/collector
regression-free; never log raw prompts/tokens/keys; unsupported≠zero; cache OFF by default; no
sidecar-to-sidecar comms.
