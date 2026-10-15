.PHONY: build test test-integration lint vuln verify run-api run-worker

build:
	go build ./...

test:
	go test -race -cover ./...

# Needs a PostgreSQL: docker compose up -d postgres
test-integration:
	TEST_DATABASE_URL=postgres://orders:orders@127.0.0.1:5432/orders?sslmode=disable go test -race -count=1 ./internal/postgres/...

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	golangci-lint run

vuln:
	govulncheck ./...

# The single local gate, same as CI.
verify: lint test vuln

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker
