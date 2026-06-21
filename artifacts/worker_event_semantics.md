# Worker Event Semantics — observation vs inference

A pure host sidecar observes compute-process **count** and **memory** deltas. It CANNOT, from those
deltas alone, prove whether a worker crashed, was SIGKILLed, OOMed, or exited gracefully. The event
taxonomy keeps EVIDENCE separate from INFERENCE.

## Event taxonomy (`internal/core/model.go`)
| Event | Emitted by host sidecar? | Meaning |
|---|---|---|
| `WORKER_OBSERVED` | yes | a compute process is present (first sighting) |
| `WORKER_STARTED` | yes | process count / memory increased |
| `WORKER_DISAPPEARED` | yes | count/memory decreased — **termination_cause = unknown** |
| `WORKER_EXIT_OBSERVED` | only with supervision | supervised exit status seen |
| `WORKER_CRASH_CONFIRMED` | **only with direct evidence** | confirmed abnormal exit |
| `WORKER_OOM_CONFIRMED` | **only with direct evidence** | confirmed OOM |
| `WORKER_TERMINATION_CAUSE_UNKNOWN` | alias for unknown disappearance | |

The host sidecar emits ONLY `WORKER_OBSERVED`/`WORKER_STARTED`/`WORKER_DISAPPEARED`.
`WORKER_CRASH_CONFIRMED`/`WORKER_OOM_CONFIRMED` exist as constants and are emitted ONLY when a future
integration supplies direct evidence: supervised exit status, cgroup/container-runtime event,
runtime-specific failure signal, or a kernel/vendor error clearly associated with the worker.

## Disappearance event shape (real H100 capture)
```json
{
  "kind": "WORKER_DISAPPEARED",
  "termination_cause": "unknown",
  "ground_truth_source": "",
  "evidence": {
    "previous_process_count": 1,
    "current_process_count": 0,
    "memory_released_bytes": 22023241728
  }
}
```

## SIGKILL experiment language (corrected)
- **Correct:** "The experiment harness issued SIGKILL to the worker, and the sidecar detected worker
  *disappearance* (process count 1→0, ~22GB released) after X ms. The sidecar did NOT and cannot
  independently determine the cause was SIGKILL."
- **Forbidden:** "The sidecar independently identified a SIGKILL crash."

## Stability-score input (corrected)
The former `ProcessCrashes` stability input (which was never populated — a dead, misleading field)
is renamed `AbnormalDisappearances`: a count of unexplained worker disappearances in the window. It
is an OBSERVED signal, not a confirmed-crash count, and applies a mild penalty only.

## Vendor note (AMD)
On MI350X, `rocm-smi --showpids` per-card process attribution is unreliable — `compute_proc_count`
for the busy card frequently stays 0 even with a live 25GB allocation (confirmed round 1 + round 2).
Worker detection on AMD therefore leans on the memory-delta evidence. NVIDIA per-device proc counts
(`--query-compute-apps`) are accurate.
