.PHONY: test build run compose-up compose-down

test:
	docker run --rm -v "$(CURDIR):/src" -w /src golang:1.24-alpine go test ./...

build:
	docker build -t model-gateway:dev .

run:
	docker compose up --build gateway mock-model-1 mock-model-2

compose-up:
	docker compose --profile infra up --build

compose-down:
	docker compose --profile infra down
