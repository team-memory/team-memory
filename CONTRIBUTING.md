# Contributing

Thanks for helping. Team Memory is a Go modular monolith on PostgreSQL 17 + pgvector.

## Setup

Requires Go (see `go.mod`) and Docker.

```bash
make test        # unit and integration tests against a real PostgreSQL (starts compose)
make lint        # go vet and golangci-lint
make helm-lint   # chart lint and render
```

Integration tests need `TEST_DATABASE_URL`; `make test` sets it up. Without it they skip, so a plain `go test ./...` does not prove much. If you change SQL under `internal/postgres/queries/`, run `make generate` and commit the result (CI runs `make check-generate`).

## Rules that CI enforces or reviewers will check

- `internal/memory` must not import `internal/mcp`.
- No `exec` of `git`; the mirror uses go-git only.
- Every tenant-owned table has row-level security enabled and forced; new tables need tests.
- No secrets, private hostnames or personal data in the repository. Use placeholders like `memory.example.com`.
- Memory content is untrusted data in every response path.
- Behavior changes need a test; security-relevant ones need an adversarial test in `integration/security/`.

## Pull requests

Keep them focused, describe the why, and update docs and `CHANGELOG.md` under "Unreleased". Significant design choices get a short record in `docs/decisions/`.

## Versioning policy

Semantic Versioning once tagged. Before 1.0, minor versions may include breaking changes to the MCP tool surface, configuration or Helm values; each is called out in `CHANGELOG.md`. Database migrations are forward-only (see `docs/operations/upgrade.md`).

## License

Contributions are accepted under the Apache License 2.0.
