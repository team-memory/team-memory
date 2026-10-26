SHELL := /bin/bash
DATABASE_URL ?= postgres://teammemory:teammemory-dev@127.0.0.1:55432/teammemory?sslmode=disable
TEST_DATABASE_URL ?= postgres://teammemory:teammemory-dev@127.0.0.1:55432/postgres?sslmode=disable
SQLC_VERSION := v1.31.1
export DATABASE_URL TEST_DATABASE_URL

.PHONY: docs-screenshots image helm-lint check-alerts secret-scan dev-git-up test-mirror-remote k3d-up k3d-down rollback-drill backup-drill loadtest test-e2e help db-up db-down dev-roles migrate run worker build test test-security test-unit eval bench-vector generate check-generate lint dev-idp-up dev-idp-down

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

docs-screenshots: db-up ## Capture the synthetic portal walkthrough (same dependencies as test-e2e)
	TEAM_MEMORY_DOC_SCREENSHOTS=1 go test -count=1 -v -run '^TestPortalDocumentationScreenshots$$' ./integration/security/

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
	@if command -v golangci-lint >/dev/null; then golangci-lint run ./...; else echo "golangci-lint not installed; skipped"; fi

dev-idp-up: ## Start the disposable Keycloak + Dex stack for SSO development
	./deploy/dev-idp/setup.sh
	docker compose -f deploy/dev-idp/compose.yaml up -d --wait

dev-idp-down: ## Stop the dev IdP stack
	docker compose -f deploy/dev-idp/compose.yaml down

IMAGE ?= team-memory:dev
PROMETHEUS_IMAGE := prom/prometheus:v3.5.0

image: ## Build the container image ($(IMAGE))
	docker build --build-arg VERSION=$$(git describe --tags --always --dirty) -t $(IMAGE) .

helm-lint: ## Lint and render the chart (solo and all-features values)
	helm lint deploy/helm/team-memory --strict -f deploy/helm/team-memory/ci/solo-values.yaml
	helm lint deploy/helm/team-memory --strict -f deploy/helm/team-memory/ci/full-values.yaml
	helm template t deploy/helm/team-memory -f deploy/helm/team-memory/ci/full-values.yaml >/dev/null
	python3 deploy/helm/team-memory/ci/test_ingress.py

check-alerts: ## Validate and unit-test the example alert rules with promtool (Docker)
	docker run --rm -v $(CURDIR)/deploy:/deploy -w /deploy/observability --entrypoint promtool $(PROMETHEUS_IMAGE) \
		check rules ../helm/team-memory/files/prometheus-rules.yaml
	docker run --rm -v $(CURDIR)/deploy:/deploy -w /deploy/observability --entrypoint promtool $(PROMETHEUS_IMAGE) \
		test rules alerts-test.yaml

secret-scan: ## Scan the working tree and git history for secrets (gitleaks)
	gitleaks dir . --redact --no-banner
	gitleaks git . --redact --no-banner

dev-git-up: ## Start the disposable Gitea (HTTPS + SSH) for mirror tests
	./deploy/dev-git/setup.sh

test-mirror-remote: ## Authenticated SSH and HTTPS mirror tests against the dev Gitea
	TEAM_MEMORY_MIRROR_TEST_ENV=$(CURDIR)/deploy/dev-git/.local/env go test -count=1 -v -run 'TestMirror(HTTPS|SSH)' ./internal/mirror/

k3d-up: ## Disposable k3d cluster with Traefik, PostgreSQL and Keycloak (never touches other contexts)
	./deploy/dev-k3d/setup.sh

k3d-down: ## Delete the disposable k3d cluster
	k3d cluster delete tm-dev

rollback-drill: ## Upgrade/rollback/bad-migration drill on the k3d cluster
	./deploy/dev-k3d/rollback-drill.sh

backup-drill: ## pgBackRest restore drill in throwaway containers
	./deploy/backup/drill.sh

loadtest: ## Build the load-test tool (see docs/operations/load-test-report.md)
	go build -o bin/loadtest ./loadtest

