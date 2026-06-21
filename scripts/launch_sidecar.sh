#!/bin/bash
# One-command deployment/launch per node. Auto-detects vendor; pins to free devices if given.
# Usage: scripts/launch_sidecar.sh [port] [devices_csv]
PORT=${1:-19095}
DEVICES=${2:-}
DIR="$(cd "$(dirname "$0")/.." && pwd)"
[ -x "$DIR/bin/sidecar" ] || (cd "$DIR" && go build -o bin/sidecar ./cmd/sidecar)
ARGS=(-listen "[::]:$PORT" -poll 2s)
[ -n "$DEVICES" ] && ARGS+=(-devices "$DEVICES")
echo "launching sidecar on :$PORT devices=${DEVICES:-all}"
exec "$DIR/bin/sidecar" "${ARGS[@]}"
