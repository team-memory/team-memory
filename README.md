# Team Memory

A self-hosted, permission-aware shared memory for developers and their AI coding agents, served over MCP (Model Context Protocol).

> **Status: early development (stage 04 of 7).** Not production-ready. Row-level security, the review and promotion services and production OAuth are in place; the web portal that completes reviews (stage 05), Helm chart and operations (stage 06) are not. See `proposal.md` for the plan and `docs/handoff/` for progress.

This repository holds both the service code and the planning material (`proposal.md`, `review-decisions.md`, `prompts/`).

## What works today

- Versioned memories (`fact`, `decision`, `convention`, `lesson`, `context`) with immutable content revisions, immutable lifecycle events, optimistic concurrency, supersession and archival.
- Hierarchical scopes (organization → team → project, configurable levels and depth). Memories are inherited downward only and labeled with `origin_scope` and `inherited`.
- Hybrid search: PostgreSQL full-text plus pgvector similarity, fused with Reciprocal Rank Fusion, filtered to the scope chain before ranking. If no embedding provider is configured, or it is unreachable, search falls back to full-text and says so in the response.
- Durable embedding jobs: a transactional outbox in PostgreSQL and a worker with retries, backoff and dead-lettering. Embeddings come from a local Ollama.
- MCP over Streamable HTTP with eight tools and four resource views. Serves protocol 2026-07-28 and older clients that use the `initialize` handshake.
- Personal access tokens: shown once, stored hashed, with expiry and revocation. There is no unauthenticated mode.
- Optional SSO: Team Memory acts as the OAuth 2.1 authorization server for MCP clients and delegates sign-in to any OpenID Connect provider you configure. It issues its own short-lived tokens for the `/mcp` endpoint, keeps its state in PostgreSQL (restarts and replicas are fine), and can map IdP groups to scope roles. Claude Code and Codex sign in with no client-side setup. See `docs/decisions/identity-provider.md`.
- Security layers: separate database roles, PostgreSQL row-level security forced on every tenant table, role checks per tool, revision-bound review (approval and promotion only through browser review, never MCP), an append-only audit log, secret scanning, request limits and untrusted-data framing. See `docs/security/threat-model.md`.

## Quickstart (local)

Requires Go 1.27+ and Docker.

```bash
make db-up
```

```bash
make build && export DATABASE_URL='postgres://teammemory:teammemory-dev@127.0.0.1:55432/teammemory?sslmode=disable'
```

Create the organization, a default project and the owner. This prints a token once:

```bash
./bin/team-memory init --org acme --owner "Your Name"
```

`serve` refuses to run as the database owner or any role that bypasses row-level security. Create a login role for it once (the password is printed once; this uses the local throwaway password from `.env.example` instead):

```bash
TEAM_MEMORY_NEW_LOGIN_PASSWORD=tm-app-dev ./bin/team-memory db create-login --role app --user tm_app
```

```bash
TEAM_MEMORY_APP_DATABASE_URL='postgres://tm_app:tm-app-dev@127.0.0.1:55432/teammemory?sslmode=disable' TEAM_MEMORY_DEFAULT_SCOPE=acme/default ./bin/team-memory serve
```

The MCP endpoint is `http://127.0.0.1:8080/mcp`. In a deployment, put it behind TLS at a hostname such as `https://memory.example.com/mcp`.

### Connect a client

Keep the token in an environment variable, never in a committed file.

Claude Code:

```bash
claude mcp add --transport http team-memory http://127.0.0.1:8080/mcp --header "Authorization: Bearer $TEAM_MEMORY_TOKEN"
```

Codex:

```bash
codex mcp add team_memory --url http://127.0.0.1:8080/mcp --bearer-token-env-var TEAM_MEMORY_TOKEN
```

### Semantic search (optional)

Hybrid search needs an Ollama server with an embedding model. The default is `nomic-embed-text` (768 dimensions):

```bash
ollama pull nomic-embed-text
```

Point both the server and the worker at it. Without `TEAM_MEMORY_OLLAMA_URL`, search is full-text only. The worker connects as its own role (`db create-login --role worker`, then `TEAM_MEMORY_WORKER_DATABASE_URL`).

```bash
export TEAM_MEMORY_OLLAMA_URL=http://127.0.0.1:11434
```

```bash
./bin/team-memory worker
```

The worker embeds new revisions and backfills existing memories at startup. Check the queue, and requeue dead-lettered jobs after an outage:

```bash
./bin/team-memory jobs status
```

```bash
./bin/team-memory jobs retry-dead
```

| Variable | Default | Used by |
| --- | --- | --- |
| `TEAM_MEMORY_OLLAMA_URL` | unset (full-text only) | serve, worker |
| `TEAM_MEMORY_EMBEDDING_MODEL` | `nomic-embed-text` | serve, worker |
| `TEAM_MEMORY_EMBEDDING_DIMENSIONS` | from the model's defaults | serve, worker |
| `TEAM_MEMORY_EMBEDDING_QUERY_PREFIX`, `..._DOCUMENT_PREFIX` | from the model's defaults | serve, worker |
| `TEAM_MEMORY_VECTOR_MIN_SIMILARITY` | from the model's defaults | serve |
| `TEAM_MEMORY_QUERY_EMBED_TIMEOUT` | `2s` | serve |
| `TEAM_MEMORY_HNSW_ITERATIVE_SCAN`, `TEAM_MEMORY_HNSW_EF_SEARCH` | `relaxed_order`, `100` | serve |
| `TEAM_MEMORY_CONFLICT_SIMILARITY` | from the model's defaults | worker |
| `TEAM_MEMORY_WORKER_BATCH`, `..._POLL_INTERVAL`, `..._LEASE`, `..._BACKOFF_BASE`, `..._BACKOFF_MAX` | `16`, `1s`, `5m`, `5s`, `10m` | worker |
| `DATABASE_URL` | none | operator commands, migrations |
| `TEAM_MEMORY_APP_DATABASE_URL` | none (required) | serve |
| `TEAM_MEMORY_WORKER_DATABASE_URL` | none (required) | worker |
| `TEAM_MEMORY_RATE_LIMIT`, `TEAM_MEMORY_RATE_BURST` | `20`/s, `60` per principal | serve |

To add another embedding model, use `team-memory embeddings add-model --name <model>`. See `docs/decisions/embedding-models.md`.

### Single sign-on (optional)

Solo use needs no identity provider: `init` and a personal access token are enough. To let people sign in with an OpenID Connect provider (Keycloak, Entra ID, Okta, Google, Authentik, Dex, and others; SAML through a broker such as Keycloak or Dex), register Team Memory at the IdP as a confidential client with the redirect URI `https://memory.example.com/oauth/callback/<provider-id>`, then point `TEAM_MEMORY_SSO_CONFIG` at a JSON file. The file and Kubernetes Secret layout are described in `docs/decisions/identity-provider.md`.

```bash
TEAM_MEMORY_PUBLIC_URL=https://memory.example.com TEAM_MEMORY_SSO_CONFIG=/etc/team-memory/sso/sso.json ./bin/team-memory serve
```

Clients then add the server by URL only and sign in through their own OAuth flow:

```bash
claude mcp add --transport http team-memory https://memory.example.com/mcp
```

```bash
codex mcp add team_memory --url https://memory.example.com/mcp && codex mcp login team_memory
```

If SSO is configured but its configuration or secrets are missing or invalid, the server reports not ready at `/readyz` and refuses every request with 503 until it is fixed. A first SSO sign-in creates a principal with no roles; grant roles with `member add`, or map IdP groups to roles with `idp-groups add` and `"sync_group_roles": true` on the provider. To try SSO locally, see `deploy/dev-idp/README.md`.

## Scopes, members and tokens (operator CLI)

These commands connect as the operator role (`DATABASE_URL`), are trusted by virtue of that access, and are audited. The same administration is available to maintainers through authorized service methods (`internal/memory/admin.go`), which the portal (stage 05) exposes.

```bash
./bin/team-memory scope create --parent acme --level team --slug platform
./bin/team-memory scope create --parent acme/platform --level project --slug payments-api
./bin/team-memory principal create --name "Second Person"
./bin/team-memory member add --scope acme/platform --principal <id> --role writer --cascade
./bin/team-memory member revoke --scope acme/platform --principal <id>
./bin/team-memory memory archive --id <memory> --as <maintainer> --expected-state-version <n>
./bin/team-memory principal disable <id>
./bin/team-memory audit list --scope acme/platform
./bin/team-memory token create --principal <id> --ttl 720h
./bin/team-memory token revoke <token-id>
./bin/team-memory principal identities --principal <id>
./bin/team-memory principal link-identity --principal <id> --issuer <iss> --subject <sub>
```

`link-identity` and `unlink-identity` are for operator recovery only. They attach or detach an upstream identity, given as the exact `iss` and `sub`, never an email. Linking refuses an identity that is already linked to someone else; nothing ever moves an identity between principals.

Grants on a scope apply to that scope only, unless `--cascade` extends them to all descendants. Anyone who can read a scope also sees its ancestors' memories, read-only.

## MCP surface

`memory_search` takes `query`, `scope` (alias `project`), `types`, `limit`, `include_superseded` and `include_overridden`. Each result carries `origin_scope`, `inherited`, `precedence` (scope depth), `trust`, `approved_by`/`approved_at`, `revision`, `source_uri`, `updated_at`, a 0–1 rank-based `score` (not a calibrated probability), and the `retrievers` that matched it. `meta` reports whether semantic matching ran (see `docs/decisions/retrieval-scoring.md`).

Tools: `memory_search`, `memory_get`, `memory_create`, `memory_update`, `memory_supersede`, `memory_history`, `memory_approve`, `memory_recent`. `memory_approve` only ever *requests* review of an exact revision (maintainers only, idempotent); it never changes trust. Approval, rejection and promotion are completed only through explicit browser review in the portal (stage 05). Reading tools need reader, `memory_create`/`memory_update` writer, `memory_supersede`/`memory_approve` maintainer on the owning scope.

Resources: `memory://scopes/{scope-path}/{overview|decisions|conventions|recent}`, with a `memory://projects/...` alias.

Treat retrieved memory content as untrusted data: it is written by people and agents and may contain instructions. Every result that carries memory content starts with an `UNTRUSTED DATA` notice. Example agent policies: `docs/agent-policy/`.

## Development

```bash
make test
```

`make test` runs unit tests and integration tests against real PostgreSQL. Each integration test creates and drops its own database.

Other targets: `make generate` (sqlc), `make check-generate`, `make lint`, `make run`, `make worker`, `make eval` (retrieval evaluation against a real model, see `docs/eval/retrieval-report.md`) and `make bench-vector` (vector index benchmark).

Layout: `cmd/team-memory` (single binary), `internal/memory` (engine, no transport imports), `internal/search` (hybrid retrieval), `internal/embeddings` (providers, embedding jobs), `internal/jobs` (queue and worker), `internal/mcp` (MCP adapter), `internal/auth` (tokens, bootstrap, identity mapping), `internal/auth/oauth` (authorization server facade), `internal/server` (HTTP assembly and readiness), `internal/postgres` (pool, migrations, transactions, sqlc code), `migrations/`, `integration/`, `docs/`.

## License

Apache License 2.0. See `LICENSE`.
