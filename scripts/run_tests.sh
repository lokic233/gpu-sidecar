#!/bin/bash
# One-command local test path.
set -e
cd "$(dirname "$0")/.."
echo "=== go vet ==="; go vet ./internal/... ./cmd/...
echo "=== go test ==="; go test ./internal/... -count=1
echo "ALL TESTS PASSED"
