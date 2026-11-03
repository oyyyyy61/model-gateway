.PHONY: test test-docker build run fmt vet compose-up compose-down

test:
	go test ./...

test-docker:
	docker run --rm -v "$(CURDIR):/src" -w /src golang:1.24-alpine go test ./...

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -o bin/gateway ./cmd/gateway
	CGO_ENABLED=0 go build -trimpath -o bin/mock-model ./cmd/mock-model

run:
	docker compose up --build gateway mock-model-1 mock-model-2

compose-up:
	docker compose --profile infra up --build

compose-down:
	docker compose --profile infra down
