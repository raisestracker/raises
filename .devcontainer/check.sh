#!/bin/sh
set -eu
cd /workspace
test "$(go env GOVERSION)" = go1.26.9
test "$(npm config get min-release-age)" = 7
npm ci --ignore-scripts --no-fund
npm run build:css
go test -race ./...
go vet ./...
mkdir -p .container-state
CGO_ENABLED=0 go build -trimpath -o .container-state/raises ./cmd/raises
DATABASE_PATH=/tmp/raises-container-check.db BASE_URL=http://127.0.0.1:8080 \
    REPORT_ENABLED=0 OPERATIONAL_ALERTS_ENABLED=0 NTFY_ENABLED=0 \
    .container-state/raises > /tmp/raises-container-check.log 2>&1 &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true' EXIT INT TERM
curl --fail --silent --show-error --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:8080/healthz
curl --fail --silent --show-error http://127.0.0.1:8080/readyz
