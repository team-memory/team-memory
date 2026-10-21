SHELL := /bin/bash
DATABASE_URL ?= postgres://teammemory:teammemory-dev@127.0.0.1:55432/teammemory?sslmode=disable
TEST_DATABASE_URL ?= postgres://teammemory:teammemory-dev@127.0.0.1:55432/postgres?sslmode=disable
SQLC_VERSION := v1.31.1
export DATABASE_URL TEST_DATABASE_URL

.PHONY: help db-up db-down migrate run worker build test test-unit eval bench-vector generate check-generate lint dev-idp-up dev-idp-down

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

worker: build ## Run the embedding worker (needs TEAM_MEMORY_OLLAMA_URL)
	./bin/team-memory worker

test-unit: ## Unit tests only (no database)
	go test ./internal/... ./cmd/...

test: db-up ## Unit and integration tests against real PostgreSQL
	go test -race -count=1 ./...

eval: db-up ## Retrieval evaluation (needs TEAM_MEMORY_OLLAMA_URL and TEAM_MEMORY_EVAL_MODELS)
	TEAM_MEMORY_EVAL_OUT=$(CURDIR)/docs/eval go test -count=1 -v -timeout 60m ./integration/eval/

bench-vector: db-up ## Vector index benchmark; TEAM_MEMORY_VECTOR_BENCH=20000,100000 sets sizes
	TEAM_MEMORY_VECTOR_BENCH=$${TEAM_MEMORY_VECTOR_BENCH:-20000,100000} TEAM_MEMORY_BENCH_OUT=$(CURDIR)/docs/eval/vectorbench-results.json \
		go test -count=1 -v -timeout 120m ./integration/vectorbench/

generate: ## Regenerate sqlc code
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

check-generate: generate ## Fail if generated code is stale
	git diff --exit-code -- internal/postgres/db

lint: ## go vet and golangci-lint (if installed)
	go vet ./...
	@command -v golangci-lint >/dev/null && golangci-lint run ./... || echo "golangci-lint not installed; skipped"

dev-idp-up: ## Start the disposable Keycloak + Dex stack for SSO development
	./deploy/dev-idp/setup.sh
	docker compose -f deploy/dev-idp/compose.yaml up -d --wait

dev-idp-down: ## Stop the dev IdP stack
	docker compose -f deploy/dev-idp/compose.yaml down
