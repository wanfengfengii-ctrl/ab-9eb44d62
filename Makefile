.PHONY: up verify test build down restart clean

## Start the API (HOST_PORT=9090 make up to change the host port)
up:
	docker compose up -d --build api

## Run the one-shot verify service (exit code reports the result)
verify:
	docker compose run --rm --build verify

## Run unit tests locally
test:
	go test ./...

## Build locally
build:
	go build ./...

## Stop and remove containers (state in the named volume is kept)
down:
	docker compose down

## Restart the API (accepted state survives; volume is kept)
restart:
	docker compose restart api

## Stop and also delete the persisted state
clean:
	docker compose down -v
