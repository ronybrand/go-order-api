.PHONY: build test test-integration lint vuln verify run-api run-worker

build:
	go build ./...

# -p 1: internal/postgres and internal/httpapi share the same database and TRUNCATE it in their setup.
test:
	go test -p 1 -race -count=1 -coverprofile=coverage.out ./internal/...
	sh scripts/check-coverage.sh coverage.out 80

# Needs a PostgreSQL: docker compose up -d postgres
test-integration:
	TEST_DATABASE_URL=postgres://orders:orders@127.0.0.1:5432/orders?sslmode=disable go test -p 1 -race -count=1 ./internal/postgres/... ./internal/httpapi/...

# golangci-lint já roda gofmt e govet, então não há `go vet` separado.
lint:
	gofmt -l . | (! grep .)
	golangci-lint run

vuln:
	govulncheck ./...

# The single local gate, same as CI.
verify: lint test vuln

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker
