#!/bin/sh
# One-shot verification gate, run by the "verify" compose service:
#   1. build checks (go build + go vet)
#   2. unit/integration tests (go test)
#   3. live smoke: publish, evolve, conflict and concurrency checks against
#      the running API
# Exits 0 only if every step passes; safe to run repeatedly.
set -eu

cd "$(dirname "$0")"

echo "== [1/3] build checks =="
go build -buildvcs=false ./...
go vet -buildvcs=false ./...

echo "== [2/3] unit tests =="
go test -buildvcs=false ./...

echo "== [3/3] smoke against ${API_ADDR:-http://api:8080} =="
smoke -addr "${API_ADDR:-http://api:8080}" -wait "${API_WAIT:-60s}"

echo "VERIFY OK"
