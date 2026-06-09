.PHONY: build run-api run-worker run-reconciler migrate test lint fmt up down

# Build all binaries.
build:
	go build ./...

# Run the API server (requires env vars from .env or environment).
run-api:
	go run ./cmd/api

# Run the worker.
run-worker:
	go run ./cmd/worker

# Run the reconciler.
run-reconciler:
	go run ./cmd/reconciler

# Apply database migrations.
migrate:
	go run ./cmd/migrate

# Run all tests.
test:
	go test -v -race -count=1 ./...

# Vet and format check.
lint:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

# Format all Go files.
fmt:
	gofmt -w .

# Bring up local stack (api + worker + postgres + valkey).
up:
	docker compose -f deploy/docker-compose.yml up --build

# Tear down local stack.
down:
	docker compose -f deploy/docker-compose.yml down
