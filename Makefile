SHELL := /bin/bash
DATABASE_URL ?= postgres://teammemory:teammemory-dev@127.0.0.1:55432/teammemory?sslmode=disable
TEST_DATABASE_URL ?= postgres://teammemory:teammemory-dev@127.0.0.1:55432/postgres?sslmode=disable
SQLC_VERSION := v1.31.1
export DATABASE_URL TEST_DATABASE_URL

.PHONY: help db-up db-down migrate run build test test-unit generate check-generate lint

help: ## List targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

db-up: ## Start local PostgreSQL 17 + pgvector
	docker compose up -d --wait

db-down: ## Stop local PostgreSQL and delete its data
	docker compose down -v

build: ## Build bin/team-memory
	go build -o bin/team-memory ./cmd/team-memory

migrate: build ## Apply migrations to DATABASE_URL
	./bin/team-memory migrate

run: build ## Serve MCP on 127.0.0.1:8080 (applies migrations first)
	./bin/team-memory serve --migrate

test-unit: ## Unit tests only (no database)
	go test ./internal/... ./cmd/...

test: db-up ## Unit and integration tests against real PostgreSQL
	go test -race -count=1 ./...

generate: ## Regenerate sqlc code
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

check-generate: generate ## Fail if generated code is stale
	git diff --exit-code -- internal/postgres/db

lint: ## go vet and golangci-lint (if installed)
	go vet ./...
	@command -v golangci-lint >/dev/null && golangci-lint run ./... || echo "golangci-lint not installed; skipped"
