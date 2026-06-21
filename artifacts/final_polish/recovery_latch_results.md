# Recovery Latch Results (Round 3)

## Previous bypass
The Round-2 machine left RECOVERING by re-classifying a healthy probe. If `classifyHealthy` returned
DEGRADED (low stability) or the device looked BUSY, the machine dropped out of RECOVERING into
DEGRADED, and the next healthy probe used the "DEGRADED→READY applies immediately" rule —
**bypassing the recovery hold + healthy-streak**. Path:
```
OFFLINE → RECOVERING → DEGRADED (low stability) → READY   ❌ bypass
```
Reproduced by failing tests `TestRecoveryLatch_LowStabilityDoesNotBypass` and
`TestRecoveryLatch_StreakResetsOnInterruption` (both FAILed against baseline a24e19c).

## Corrected invariant
Added an explicit latch (`recovery_latched`, `recovery_started_at`, `recovery_healthy_streak`):
- Entering OFFLINE (hard OR soft-threshold) sets `recovery_latched = true`.
- While latched, the externally visible state stays **RECOVERING**. Low stability / high util /
  transient soft degradation do NOT clear the latch — they only annotate `reason_codes`.
- The latch releases (→ READY/BUSY) ONLY when BOTH:
  - elapsed ≥ `RecoveringHoldSec` (5s), AND
  - `recovery_healthy_streak ≥ RecoveryStreak` (3 consecutive healthy probes).
- A soft failure during recovery resets `recovery_healthy_streak` to 0 but keeps the latch (stays RECOVERING).
- `OfflineFailures` consecutive soft failures, or any hard evidence, returns to OFFLINE and re-latches.
- DRAINING is explicit and does not clear recovery history.

**Guarantee:** no path through DEGRADED, BUSY, or any intermediate classification can promote a
post-OFFLINE device to READY without satisfying hold + streak. Proven by 5 table-driven tests.

## Real-hardware traces
- **H100 (dev6)**: OFFLINE(latched) → RECOVERING streak 1→2→3→4 → READY at `RECOVERY_STREAK_MET`
  (`h100_smoke/recovery_latch_trace.json`).
- **MI350X (dev5)**: OFFLINE(latched) held ~8s → RECOVERING streak 1→2→3 → READY at
  `RECOVERY_STREAK_MET` (`mi350x_smoke/recovery_latch_trace.json`). AMD's slower rocm-smi cadence
  visibly lengthens the OFFLINE→RECOVERING edge but the latch invariant holds identically.
