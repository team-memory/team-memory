SHELL := /bin/bash
DATABASE_URL ?= postgres://teammemory:teammemory-dev@127.0.0.1:55432/teammemory?sslmode=disable
TEST_DATABASE_URL ?= postgres://teammemory:teammemory-dev@127.0.0.1:55432/postgres?sslmode=disable
SQLC_VERSION := v1.31.1
export DATABASE_URL TEST_DATABASE_URL

.PHONY: test-e2e help db-up db-down dev-roles migrate run worker build test test-security test-unit eval bench-vector generate check-generate lint dev-idp-up dev-idp-down

help: ## List targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

db-up: ## Start local PostgreSQL 17 + pgvector
	docker compose up -d --wait

db-down: ## Stop local PostgreSQL and delete its data
	docker compose down -v

build: ## Build bin/team-memory
	go build -o bin/team-memory ./cmd/team-memory

dev-roles: migrate ## Create local login roles tm_app and tm_worker (throwaway passwords from .env.example)
	TEAM_MEMORY_NEW_LOGIN_PASSWORD=tm-app-dev ./bin/team-memory db create-login --role app --user tm_app || true
	TEAM_MEMORY_NEW_LOGIN_PASSWORD=tm-worker-dev ./bin/team-memory db create-login --role worker --user tm_worker || true

migrate: build ## Apply migrations to DATABASE_URL
	./bin/team-memory migrate

run: build ## Serve MCP on 127.0.0.1:8080 as tm_app (applies migrations first; run make dev-roles once)
	TEAM_MEMORY_APP_DATABASE_URL=$${TEAM_MEMORY_APP_DATABASE_URL:-postgres://tm_app:tm-app-dev@127.0.0.1:55432/teammemory?sslmode=disable} ./bin/team-memory serve --migrate

worker: build ## Run the embedding worker as tm_worker (needs TEAM_MEMORY_OLLAMA_URL)
	TEAM_MEMORY_WORKER_DATABASE_URL=$${TEAM_MEMORY_WORKER_DATABASE_URL:-postgres://tm_worker:tm-worker-dev@127.0.0.1:55432/teammemory?sslmode=disable} ./bin/team-memory worker

test-security: db-up ## Adversarial security suite only
	go test -race -count=1 ./integration/security/

test-e2e: db-up ## Portal browser tests in Chromium (first: cd e2e && npm ci && npx playwright install chromium)
	TEAM_MEMORY_E2E=1 go test -count=1 -v -run TestPortalBrowserEndToEnd ./integration/security/

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
