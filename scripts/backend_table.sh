#!/bin/bash
# One-command normalized backend table across both sidecars over the mesh.
# Usage: scripts/backend_table.sh [h100_url] [mi350x_url]
# Defaults assume local H100 sidecar + MI350X over IPv6 mesh.
H100=${1:-http://[::1]:19095}
MI350X=${2:-http://[2401:db00:272c:590b:face:0:133:0]:19095}
DIR="$(cd "$(dirname "$0")/.." && pwd)"
exec "$DIR/bin/collector" -once -format table -sidecars "h100=$H100,mi350x=$MI350X"
