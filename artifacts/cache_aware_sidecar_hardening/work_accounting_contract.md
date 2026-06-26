# Work-Accounting Contract (P0 #4)

Token-level prefill/decode reservations with a correct lifecycle tied to the request TICKET. ADVISORY
in this task (the request-count + inflight limits remain the hard safety bounds), but semantically
correct so it can later back admission credits.

Source: `internal/dataplane/work.go`, `internal/dataplane/proxy.go`, `internal/dataplane/queue.go`.

## Lifecycle (was: reserve-at-dispatch + recompute; now: reserve-at-admission + ticket-carried)
```
ADMISSION (queue entry):  Reserve(...)  -> booked in the QUEUED bucket, handle stored on the ticket
DISPATCH:                 reservation.Activate()  -> moved QUEUED -> ACTIVE
TERMINAL (every path):    reservation.Release()   -> removed from whichever bucket; idempotent
```
`resolveTerminal(ticket, ready)` releases the reservation exactly once on EVERY terminal path:
successful stream, successful JSON, upstream connect failure, pre-first-token failure, partial
stream, cancellation (in-queue / pre-first-token / mid-stream), queue timeout, runtime non-2xx,
no-flusher.

## Conservative reservation sizing (at admission)
```
READY match (trustworthy, high confidence): reserve UNCACHED prompt tokens  (input - matched_ready)
ABSENT / WARMING / unknown / stale / low-confidence: reserve FULL prompt tokens
```
Sizing reads the LOCAL residency state (`CacheObserver.LookupState`) — only a READY prefix shrinks the
reservation. WARMING is treated as "no usable match yet" → full prompt reserved (conservative).

## Counters (exposed via /v1/queue.work_accounting)
```
queued_reserved_prefill_tokens     active_prefill_tokens
queued_reserved_decode_tokens      active_decode_tokens
total_outstanding_prefill_tokens   total_outstanding_decode_tokens   (= queued + active)
lifetime_reserved_prefill_tokens   lifetime_reserved_decode_tokens   (monotonic)
```

## Invariants (tested)
- `outstanding == queued + active` (by construction in Snapshot()).
- `queued >= 0`, `active >= 0`, `outstanding >= 0` (clamped; asserted in tests).
- Activate moves queued→active without changing outstanding.
- Release frees from the correct bucket; double-release is a no-op (never negative).
- After ALL requests terminate, every counter returns to 0; lifetime totals advanced.
- Race-clean under concurrent activate/release/cancel (TestWorkAccountant_ConcurrentReleaseRaceClean).
