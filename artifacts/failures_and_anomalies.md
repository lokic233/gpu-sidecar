# Failures & Anomalies (unsanitized)

Real problems found during implementation and validation. Negative results are kept.

## 1. AMD `amd-smi` is permission-blocked (vendor-tool asymmetry)
`amd-smi metric/list` fail with `RuntimeError: User is missing the following required groups:
render, video`. This is the *richest* AMD interface (per-engine util, detailed ECC, throttle
status). The sidecar degrades to `rocm-smi --json` which is available but exposes fewer fields.
**Impact:** `power_limit`, fine-grained ECC, and per-engine util are unavailable on AMD here.
Marked `supported=false`, not faked. A production deploy should add the user to render/video
or run amd-smi via a privileged helper.

## 2. AMD device numbering is a non-identity permutation (3 different schemes)
On the MI350X, `HIP_VISIBLE_DEVICES=N` does NOT map to `rocm-smi cardN`:
- Empirically: HIP ordinal 4 → physically lands on `rocm-smi card7`; HIP 3 → card1.
- `rocm-smi --showpids` reports a THIRD "GPU(s)" id that matched neither.
NVIDIA has no such split (CUDA index == nvidia-smi index == compute-apps gpu_uuid).
**Impact:** the first MI350X detection matrix was fully contaminated (workload pinned to a card
the experiment watched the wrong index of → all detections None). Root-caused, then fixed with a
vendor-agnostic `detect_auto.py` that watches *which* device the sidecar sees change.
**Lesson:** never assume cross-vendor device-index equivalence; detect the affected device empirically.

## 3. AMD per-card compute-process attribution is unreliable
`rocm-smi --showpids` consistently reported the workload's GPU column inconsistently and the
sidecar's `compute_proc_count` for the busy card frequently stayed 0 even with a 25-30GB live
allocation. Worker-start detection on AMD therefore falls back to the **memory-delta** signal
(reliable) rather than proc-count (unreliable). On NVIDIA, `--query-compute-apps` per-device
proc count is accurate. **This is a genuine vendor signal-quality gap**, documented in the matrix.

## 4. AMD telemetry is laggier than NVIDIA after process exit
Worker-stop detection: H100 median 1.76s vs MI350X 4.5-10.5s. AMD VRAM "used" readout takes
several seconds to fall after a process exits (driver/SMI accounting lag), and OFFLINE detection
after fault injection was 5.0s on AMD vs 1.5s on NVIDIA. The sidecar reports truthfully; the
underlying tool is just slower to update.

## 5. Pipe-buffer deadlock in the first detection harness (my bug)
`subprocess.Popen(stdout=PIPE)` without draining → the workload's heartbeat prints filled the
64KB pipe and the workload blocked, so "stop" never fired. Fixed by routing workload stdout to
DEVNULL. Caught because stop_delay was always None. (Classic, but worth recording.)

## 6. Leaked GPU workloads from a timed-out foreground run
An early experiment exceeded the 30s foreground cap and left a `workload_cuda` holding 32GB on
GPU 4, polluting the next run's baseline (procs already=1). Detected via baseline inspection.
**Lesson:** always verify a clean baseline before a detection experiment; clean up by exact
binary path, never broad `pkill -f workload` (it self-matches the launching command).

## 7. Process-supervision friction on the CLI mesh
Foreground commands are killed when the shell exits; broad `pkill -f '/tmp/sidecar'` matched
sibling shells and SIGKILLed unrelated jobs. Sidecars must run as tracked background processes;
stop them by port (`fuser -k PORT/tcp`), not by name pattern. This is an operational lesson, not
a sidecar defect — but it shaped the launch/stop scripts.

## 8. `cpu_pct=0.0` artifact in early overhead samples
Short 2s sampling windows on a 384-core MI350X rounded the sidecar's tiny CPU share to 0.0%.
Re-measured over 8s: **0.25% of one core**. Not a real anomaly — a measurement-resolution issue.

## Non-anomalies (verified correct, not bugs)
- H100 GPUs 2,5 show 77-88GB used at 0% util: other users' resident-but-idle jobs. Sidecar
  correctly reports them; we simply don't run workloads there.
- Stability score never returned to baseline within 45s after recovery — **intended** (memory of
  instability via asymmetric EWMA), not a stuck value.
