# Telemetry Schema Registry

A small, dependency-free service that centrally publishes binary field
schemas for telemetry subjects. It guarantees that field numbers of
deleted fields are never reused with a new meaning, and that concurrent
publishes can never fork the current version of a subject.

## Guarantees

- **Linear version chain** — the first version of a subject must be `1`;
  every later version must be exactly `current + 1`. Concurrent publishes
  of the same successor version succeed at most once.
- **Stable field numbers** — an existing field number can never be
  renamed or change wire type.
- **Tombstoned numbers** — deleting a field reserves its number forever.
  Reserved numbers are cumulative, are never revoked, and can never be
  reused (not even to revive the original field).
- **Idempotent publishing** — the same `requestId` with the same content
  replays the original response; the same `requestId` with different
  content is rejected with `409 REQUEST_ID_CONFLICT`.
- **Durability** — every accepted state is persisted (atomic file
  replace + fsync) before the response is returned, so it survives
  service restarts.
- **Safe failures** — a rejected request never changes the subject's
  current schema or its reserved-number tombstones.

## Wire types

Exactly four wire types are accepted: `VARINT`, `FIXED32`, `FIXED64`,
`LENGTH_DELIMITED`.

## API

### `POST /api/schemas/{subject}/versions`

```json
{
  "requestId": "client-uuid-1",
  "version": 2,
  "fields": [
    {"name": "temperature", "number": 1, "wireType": "FIXED32"},
    {"name": "humidity",    "number": 3, "wireType": "VARINT"}
  ]
}
```

- `201 Created` — version accepted. The response echoes the version and
  the cumulative `reservedNumbers`.
- A retry of the same `requestId` + content returns the original
  response with an `X-Idempotent-Replay: true` header.

### `GET /api/schemas/{subject}`

```json
{
  "subject": "telemetry.device",
  "currentVersion": 2,
  "fields": [
    {"name": "temperature", "number": 1, "wireType": "FIXED32"},
    {"name": "humidity",    "number": 3, "wireType": "VARINT"}
  ],
  "reservedNumbers": [2],
  "versions": [1, 2]
}
```

`reservedNumbers` is the cumulative set of tombstoned field numbers
(numbers of deleted fields). `404 SUBJECT_NOT_FOUND` is returned for
subjects without any accepted version.

### `GET /healthz`

Liveness/readiness probe, returns `200 {"status":"ok"}`.

### Errors

All errors share one envelope and a stable `code`:

```json
{
  "error": {
    "code": "SCHEMA_EVOLUTION_VIOLATION",
    "message": "illegal schema evolution: first violation at field number 2 (NUMBER_RESERVED)",
    "currentVersion": 2,
    "violatingNumber": 2,
    "violations": [
      {"number": 2, "rule": "NUMBER_RESERVED", "message": "field number 2 is reserved by a previously deleted field and cannot be reused"}
    ]
  }
}
```

| HTTP | Code | Meaning |
|------|------|---------|
| 400 | `INVALID_REQUEST` | Malformed body, missing fields, bad wire type, duplicate name/number |
| 404 | `SUBJECT_NOT_FOUND` | Subject has no accepted version |
| 409 | `VERSION_CONFLICT` | Version is not `current + 1` (includes losing a concurrent race) |
| 409 | `REQUEST_ID_CONFLICT` | `requestId` was already used with different content |
| 422 | `SCHEMA_EVOLUTION_VIOLATION` | Rename / re-type / reserved-number reuse; carries `currentVersion`, the smallest `violatingNumber` and all `violations` |
| 500 | `INTERNAL` | Unexpected server-side failure |

Violation rules: `FIELD_RENAMED`, `WIRE_TYPE_CHANGED`, `NUMBER_RESERVED`.

## Run with Docker Compose

```sh
docker compose up -d --build api          # start the API on localhost:8080
HOST_PORT=9090 docker compose up -d api   # ... or on a custom host port
curl localhost:8080/healthz               # health check
```

State is persisted in the `schema-data` named volume; restarting or
recreating the container keeps all accepted versions, tombstones and
idempotency records (`docker compose down -v` wipes them).

## Verify (one-shot, repeatable)

The `verify` service waits for the API to become healthy, then runs
`go vet`, `go test`, `go build` and a live smoke suite (publish, evolve,
illegal evolution, idempotent replay, requestId conflict, concurrent
races). It exits `0` only if everything passes:

```sh
docker compose run --rm --build verify; echo "exit=$?"
# or: docker compose up --build --exit-code-from verify verify
```

Re-running is safe: every run uses fresh, unique subjects, so repeated
runs against the same long-lived API instance keep passing.

## Local development

```sh
go test ./...        # unit + integration tests (persistence, concurrency)
go run ./cmd/server  # serve on :8080 (PORT, DATA_FILE env vars)
sh scripts/verify.sh # full pipeline against http://localhost:8080
```

## Layout

```
cmd/server      API server entrypoint
cmd/verify      one-shot smoke suite (used by scripts/verify.sh)
internal/registry   domain logic: versions, tombstones, idempotency, store
internal/httpserver HTTP routing and JSON envelope
scripts/verify.sh   vet + test + build + smoke pipeline
Dockerfile          multi-stage: build / runtime / verify targets
docker-compose.yml  api service (health-checked) + one-shot verify service
```
