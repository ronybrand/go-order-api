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

# golangci-lint já roda govet, então não há `go vet` separado. O gofmt fica porque é barato e
# também pega .go fora de qualquer pacote compilável.
lint:
	gofmt -l . | (! grep .)
	golangci-lint run

# Versão fixada igual à do CI: um govulncheck antigo no PATH não pode fazer o gate local passar.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Gate local: os mesmos jobs lint, test e vuln do CI. O CI ainda builda e escaneia as imagens
# Docker (job image), que não fazem sentido no fluxo local.
verify: lint test vuln

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker
