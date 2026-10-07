#!/bin/sh
# One-shot verification pipeline:
#   1. go vet      (static checks)
#   2. go test     (unit / integration tests, incl. persistence & concurrency)
#   3. go build    (build check for every package)
#   4. smoke suite (live publish / evolve / conflict / idempotency / race)
# Exits non-zero on the first failing step; prints a summary at the end.
# -buildvcs=false keeps the checks independent of any VCS metadata state.
set -eu

cd "$(dirname "$0")/.."

echo "==> [1/4] go vet"
go vet -buildvcs=false ./...

echo "==> [2/4] go test"
go test -buildvcs=false ./...

echo "==> [3/4] go build"
go build -buildvcs=false ./...

BASE="${API_BASE_URL:-http://localhost:8080}"
echo "==> [4/4] smoke tests against ${BASE}"
go run -buildvcs=false ./cmd/verify -base-url "${BASE}"

echo "==> ALL CHECKS PASSED"
