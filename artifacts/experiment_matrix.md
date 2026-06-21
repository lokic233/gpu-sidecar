# Experiment Matrix

All experiments use real GPUs on shared hosts; workloads pinned to free devices, conservative
memory caps (≤30GB of 97/288GB). Raw artifacts under h100_raw/, mi350x_raw/, mesh_raw/, time_series/.

| # | Experiment | Script | Node(s) | Reps | What it validates | Result artifact |
|---|---|---|---|---|---|---|
| 1 | Idle baseline / live telemetry | sidecar /v1/status | both | continuous | normalized contract on real HW | h100_status.json, mi350x_status.json |
| 2 | Workload start→BUSY→stop detection | detect_experiment.py | H100 | 9 | lifecycle detection latency | h100_raw/detect_*.json |
| 3 | Vendor-agnostic auto-detect | detect_auto.py | both | 5+ | detection w/ AMD device-map quirk | */autodetect_*.json |
| 4 | SIGKILL crash detection | crash_experiment.py | both | 1+ | abrupt worker disappearance | */crash_*.json |
| 5 | Stability trajectory (fault inject) | stability_trajectory.py | both | 1 each | score decay/recovery + lifecycle churn | */stability_trajectory_*.json |
| 6 | Mesh disconnect/rejoin | mesh_disconnect.py | H100 | 1 | collector heartbeat loss + rejoin | mesh_raw/mesh_disconnect_*.json |
| 7 | Overhead (CPU/RSS/API/poll) | overhead.py | both | 80 API samples | sidecar cost | */overhead_*.json, overhead_results.csv |
| 8 | Cross-vendor field availability | live status diff | both | 1 | schema parity / vendor gaps | cross_vendor_matrix.csv |
| 9 | Parser/state/exec failure paths | go test | both | unit | defensive correctness | (CI: go test ./internal/...) |

## Reproduction (one-command paths)
```bash
scripts/run_tests.sh                                  # unit + vet
scripts/launch_sidecar.sh 19095 3,4,6,7               # launch sidecar (auto vendor)
scripts/backend_table.sh <h100_url> <mi350x_url>      # normalized mesh table
bash experiments/run_matrix.sh h100 ./bin/workload_cuda CUDA_VISIBLE_DEVICES 4 5 artifacts/h100_raw
bash experiments/run_matrix_auto.sh mi350x ./bin/workload_hip HIP_VISIBLE_DEVICES 4 5 artifacts/mi350x_raw 0,1
SIDECAR=... DEVICE=6 FAULT_FILE=/tmp/sidecar_fault python3 experiments/stability_trajectory.py artifacts/time_series
```
