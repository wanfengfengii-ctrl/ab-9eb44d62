# syntax=docker/dockerfile:1

# ---- build stage: compiles the server and the smoke tester ----
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/server . \
    && CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/smoke ./smoke

# ---- runtime stage: the schema registry API ----
FROM alpine:3.20 AS runtime
RUN adduser -D -u 10001 app \
    && mkdir -p /data && chown app:app /data
USER app
COPY --from=build /out/server /usr/local/bin/server
ENV PORT=8080 \
    DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["server"]

# ---- verify stage: one-shot test / build-check / smoke runner ----
FROM golang:1.23-alpine AS verify
WORKDIR /src
COPY . .
COPY --from=build /out/smoke /usr/local/bin/smoke
RUN chmod +x verify.sh
ENTRYPOINT ["./verify.sh"]
