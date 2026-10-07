# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# build: compile the server and verify binaries (static, no CGO)
# ---------------------------------------------------------------------------
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/schema-registry ./cmd/server \
 && CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/verify ./cmd/verify

# ---------------------------------------------------------------------------
# runtime: minimal image for the API server
# ---------------------------------------------------------------------------
FROM alpine:3.20 AS runtime
RUN adduser -D -u 10001 app && mkdir -p /data && chown app:app /data
COPY --from=build /out/schema-registry /usr/local/bin/schema-registry
USER app
EXPOSE 8080
ENV PORT=8080 \
    DATA_FILE=/data/registry.json
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=12 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${PORT}/healthz" || exit 1
ENTRYPOINT ["schema-registry"]

# ---------------------------------------------------------------------------
# verify: one-shot checker. Runs go vet, go test, go build against the
# source tree and then the live smoke suite against the API service.
# Exits 0 only when every check passes.
# ---------------------------------------------------------------------------
FROM golang:1.23-alpine AS verify
WORKDIR /src
COPY . .
CMD ["sh", "scripts/verify.sh"]
