#!/bin/bash
# Kill-free matrix using the vendor-agnostic auto-detect experiment.
# Each experiment manages its own workload subprocess (graceful duration exit). No pkill.
# Usage: run_matrix_auto.sh <label> <workload> <visenv> <visdev> <reps> <outdir> <excludeCSV>
set -u
LABEL=${1:-mi350x}; WL=${2:-./bin/workload_hip}; VIS=${3:-HIP_VISIBLE_DEVICES}
VISDEV=${4:-4}; REPS=${5:-5}; OUT=${6:-artifacts/mi350x_raw}; EXCL=${7:-0,1}
mkdir -p "$OUT"
echo "=== AUTO MATRIX $LABEL pin=$VIS=$VISDEV reps=$REPS excl=$EXCL ==="
for i in $(seq 1 $REPS); do
  echo "--- auto-detect rep $i ---"
  SIDECAR=http://localhost:19095 WORKLOAD=$WL VISENV=$VIS VISDEV=$VISDEV SECS=10 MEMGB=20 EXCLUDE=$EXCL \
    python3 experiments/detect_auto.py "$OUT" 2>&1 | grep -E "SUMMARY|DETECT|GROUND"
  sleep 3
done
echo "=== overhead ==="
SIDECAR=http://localhost:19095 N=80 python3 experiments/overhead.py "$OUT" 2>&1 | grep -E "cpu_pct|rss_mb|api_|probe_|SAVED"
echo "=== AUTO MATRIX DONE $LABEL ==="
