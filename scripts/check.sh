#!/usr/bin/env bash
# Runs the checks CI runs. Use before every push.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOFLAGS="${GOFLAGS:-} -tags=goolm"
echo "==> go vet"
go vet ./...
echo "==> golangci-lint"
golangci-lint run ./...
echo "==> go test -race"
go test -race -count=1 ./...
echo "==> all checks passed"
