#!/bin/bash
# Run N repetitions of the detection experiment + 1 crash + overhead, on the given device.
# Usage: run_matrix.sh <node-label> <workload-bin> <visenv> <device> <reps> <outdir>
set -u
LABEL=${1:-h100}; WL=${2:-./bin/workload_cuda}; VIS=${3:-CUDA_VISIBLE_DEVICES}
DEV=${4:-4}; REPS=${5:-5}; OUT=${6:-artifacts/h100_raw}
mkdir -p "$OUT"
echo "=== MATRIX $LABEL dev=$DEV reps=$REPS ==="
for i in $(seq 1 $REPS); do
  echo "--- detect rep $i ---"
  SIDECAR=http://localhost:19095 DEVICE=$DEV WORKLOAD=$WL VISENV=$VIS SECS=12 MEMGB=20 \
    python3 experiments/detect_experiment.py "$OUT" 2>&1 | grep -E "SUMMARY|DETECT|GROUND" 
  sleep 2
done
echo "=== crash experiment ==="
SIDECAR=http://localhost:19095 DEVICE=$DEV WORKLOAD=$WL VISENV=$VIS MEMGB=20 RUN_BEFORE_KILL=6 \
  python3 experiments/crash_experiment.py "$OUT" 2>&1 | grep -E "SUMMARY|DETECT|GROUND"
echo "=== overhead ==="
SIDECAR=http://localhost:19095 N=80 python3 experiments/overhead.py "$OUT" 2>&1 | grep -E "cpu_pct|rss_mb|api_|probe_|SAVED"
echo "=== MATRIX DONE $LABEL ==="
