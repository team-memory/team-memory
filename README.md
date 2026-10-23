# Team Memory

A self-hosted, permission-aware shared memory for developers and their AI coding agents, served over MCP (Model Context Protocol).

> **Status: personal proof of concept.** It is built and tested, and Claude Code and Codex have been verified against it, but it is not supported and has had no external security review. Do not rely on it for production or sensitive data. Details: [release checklist](docs/release-checklist.md).

This repository holds both the service code and its planning material ([`proposal.md`](proposal.md), [`review-decisions.md`](review-decisions.md), [`prompts/`](prompts/)).

**Contents:** [What it does](#what-it-does) · [Quickstart](#quickstart) · [Connect a client](#connect-a-client) · [Semantic search](#semantic-search-optional) · [Single sign-on](#single-sign-on-optional) · [Operator CLI](#operator-cli-scopes-members-and-tokens) · [MCP surface](#mcp-surface) · [Deploying](#deploying) · [Development](#development) · [Documentation map](#documentation-map) · [Limitations](#status-and-limitations)

## What it does

- **Versioned memories** of five types (`fact`, `decision`, `convention`, `lesson`, `context`), with immutable revisions and lifecycle events, optimistic concurrency, supersession and archival.
- **Hierarchical scopes** (organization → team → project; levels and depth are configurable). Memories are inherited downward only, and each result says where it came from (`origin_scope`, `inherited`).
- **Hybrid search**: PostgreSQL full-text plus pgvector similarity, fused with Reciprocal Rank Fusion and filtered to the caller's scope chain before ranking. Without a reachable embedding provider, search falls back to full-text and says so in the response.
- **Durable embedding jobs**: a transactional outbox in PostgreSQL and a worker with retries, backoff and dead-lettering. Embeddings come from a local [Ollama](https://ollama.com).
- **MCP over Streamable HTTP** with eight tools and four resource views. Serves protocol 2026-07-28 and older clients that use the `initialize` handshake.
- **Authentication**: personal access tokens (shown once, stored hashed, expiring, revocable), and optional SSO where Team Memory is the OAuth 2.1 authorization server for MCP clients and delegates sign-in to any OpenID Connect provider. There is no unauthenticated mode.
- **Web portal** at `/portal` for browsing, searching, reviewing, approving and promoting memories ([user guide](docs/portal.md)).
- **Optional one-way git mirror**: every exportable lifecycle event becomes a commit in a private repository per scope, written with go-git (no `git` binary) ([runbook](docs/operations/mirror.md)).
- **Operations**: one distroless container image, a Helm chart with NetworkPolicies and a migration hook, Prometheus metrics, OpenTelemetry traces, example alerts, pgBackRest backups with a measured restore drill, load tests and runbooks ([`docs/operations/`](docs/operations/)).
- **Security layers**: separate database roles, PostgreSQL row-level security forced on every tenant table, role checks per tool, revision-bound review (approval and promotion happen only in the browser, never over MCP), an append-only audit log, secret scanning, request limits and untrusted-data framing ([threat model](docs/security/threat-model.md)).

## Quickstart

Pick one:

- **Docker only (no Go needed):** follow [Local Docker testing](docs/operations/local-docker.md). It runs the service and its own isolated database in containers and includes Codex connection steps.
- **From source:** the steps below. They need Go 1.27+ and Docker.

### From source

1. Start PostgreSQL 17 + pgvector (loopback port 55432) and build the binary:

   ```bash
   make db-up build
   ```

2. Point operator commands at the database. These are the throwaway local credentials from `compose.yaml`:

   ```bash
   export DATABASE_URL='postgres://teammemory:teammemory-dev@127.0.0.1:55432/teammemory?sslmode=disable'
   ```

3. Create the organization `acme`, its default project `acme/default` and the owner. This applies migrations and prints a personal access token **once**; keep it:

   ```bash
   ./bin/team-memory init --org acme --owner "Your Name"
   ```

4. Create a login role for the server. `serve` refuses to run as the database owner or any role that bypasses row-level security. In a real deployment the password is generated and printed once; locally, use the throwaway password from `.env.example`:

   ```bash
   TEAM_MEMORY_NEW_LOGIN_PASSWORD=tm-app-dev ./bin/team-memory db create-login --role app --user tm_app
   ```

   (`make dev-roles` does this for both the `tm_app` and `tm_worker` roles.)

5. Start the server. `TEAM_MEMORY_DEFAULT_SCOPE` is the scope used when a tool call names none:

   ```bash
   TEAM_MEMORY_APP_DATABASE_URL='postgres://tm_app:tm-app-dev@127.0.0.1:55432/teammemory?sslmode=disable' TEAM_MEMORY_DEFAULT_SCOPE=acme/default ./bin/team-memory serve
   ```

   (`TEAM_MEMORY_DEFAULT_SCOPE=acme/default make run` does the same and applies migrations first.)

You now have:

| Endpoint | URL |
| --- | --- |
| MCP | `http://127.0.0.1:8080/mcp` (bearer token required) |
| Portal | `http://127.0.0.1:8080/portal/` (sign in with the token from step 3) |
| Health and readiness | `http://127.0.0.1:9090/healthz`, `http://127.0.0.1:9090/readyz` |

The service root (`/`) provides a public homepage with agent setup instructions and a portal link. Setup is also always available at `/welcome`, including when the portal is mounted at `/`. Set `TEAM_MEMORY_PUBLIC_URL` so the examples show your deployment’s MCP endpoint.

In a deployment, put the service behind TLS at a hostname such as `https://memory.example.com`.

## Connect a client

Put the token from `init` in an environment variable; never commit it:

```bash
export TEAM_MEMORY_TOKEN='<token printed by init>'
```

Claude Code (this stores the expanded token in Claude Code's own config):

```bash
claude mcp add --transport http team-memory http://127.0.0.1:8080/mcp --header "Authorization: Bearer $TEAM_MEMORY_TOKEN"
```

Codex (this stores only the variable name; launch Codex from a shell where it is set):

```bash
codex mcp add team_memory --url http://127.0.0.1:8080/mcp --bearer-token-env-var TEAM_MEMORY_TOKEN
```

Restart the client session after adding the server, then ask it to search Team Memory. More detail and troubleshooting: [Claude Code](docs/clients/claude-code.md), [Codex](docs/clients/codex.md). For agent instructions to add to your repositories, see [`docs/agent-policy/`](docs/agent-policy/).

## Semantic search (optional)

Without an embedding model, search is full-text only. To enable hybrid search, run an Ollama server with an embedding model. The default is `nomic-embed-text` (768 dimensions):

```bash
ollama pull nomic-embed-text
```

Set `TEAM_MEMORY_OLLAMA_URL` for **both** `serve` and the embedding worker:

```bash
export TEAM_MEMORY_OLLAMA_URL=http://127.0.0.1:11434
```

The worker connects as its own database role. Create it once (`make dev-roles` does this locally), then run the worker with `TEAM_MEMORY_WORKER_DATABASE_URL`:

```bash
TEAM_MEMORY_NEW_LOGIN_PASSWORD=tm-worker-dev ./bin/team-memory db create-login --role worker --user tm_worker
```

```bash
TEAM_MEMORY_WORKER_DATABASE_URL='postgres://tm_worker:tm-worker-dev@127.0.0.1:55432/teammemory?sslmode=disable' ./bin/team-memory worker
```

The worker embeds new revisions and backfills existing memories at startup. Check the queue, and requeue dead-lettered jobs after an outage ([runbook](docs/operations/dead-letters.md)):

```bash
./bin/team-memory jobs status
```

```bash
./bin/team-memory jobs retry-dead
```

To add another embedding model, use `team-memory embeddings add-model --name <model>` (see [embedding models](docs/decisions/embedding-models.md)).

### Configuration reference

| Variable | Default | Used by |
| --- | --- | --- |
| `DATABASE_URL` | none | operator commands, migrations |
| `TEAM_MEMORY_APP_DATABASE_URL` | none (required) | serve |
| `TEAM_MEMORY_WORKER_DATABASE_URL` | none (required) | worker |
| `TEAM_MEMORY_LISTEN` | `127.0.0.1:8080` | serve |
| `TEAM_MEMORY_METRICS_LISTEN` | `127.0.0.1:9090` (never expose publicly) | serve, worker, mirror worker |
| `TEAM_MEMORY_DEFAULT_SCOPE` | none (tool calls must name a scope) | serve |
| `TEAM_MEMORY_PUBLIC_URL` | none; the exact browser origin, used as the OAuth issuer with SSO | serve |
| `TEAM_MEMORY_RATE_LIMIT`, `TEAM_MEMORY_RATE_BURST` | `20`/s, `60` per principal | serve |
| `TEAM_MEMORY_TRUSTED_PROXIES` | none | serve |
| `TEAM_MEMORY_OLLAMA_URL` | unset (full-text only) | serve, worker |
| `TEAM_MEMORY_EMBEDDING_MODEL` | `nomic-embed-text` | serve, worker |
| `TEAM_MEMORY_EMBEDDING_DIMENSIONS` | from the model's defaults | serve, worker |
| `TEAM_MEMORY_EMBEDDING_QUERY_PREFIX`, `TEAM_MEMORY_EMBEDDING_DOCUMENT_PREFIX` | from the model's defaults | serve, worker |
| `TEAM_MEMORY_VECTOR_MIN_SIMILARITY` | from the model's defaults | serve |
| `TEAM_MEMORY_QUERY_EMBED_TIMEOUT` | `2s` | serve |
| `TEAM_MEMORY_HNSW_ITERATIVE_SCAN`, `TEAM_MEMORY_HNSW_EF_SEARCH` | `relaxed_order`, `100` | serve |
| `TEAM_MEMORY_CONFLICT_SIMILARITY` | from the model's defaults | worker |
| `TEAM_MEMORY_WORKER_BATCH`, `_POLL_INTERVAL`, `_LEASE`, `_BACKOFF_BASE`, `_BACKOFF_MAX` | `16`, `1s`, `5m`, `5s`, `10m` | worker |

Portal (`TEAM_MEMORY_PORTAL_*`), SSO, mirror and tracing settings are listed with comments in [`.env.example`](.env.example) and described in [authentication](docs/operations/authentication.md), [portal design](docs/decisions/portal.md), [mirror](docs/operations/mirror.md) and [observability](docs/operations/observability.md).

## Single sign-on (optional)

Solo use needs no identity provider: `init` and a personal access token are enough.

To let people sign in with an OpenID Connect provider (Keycloak, Entra ID, Okta, Google, Authentik, Dex and others; SAML through a broker such as Keycloak or Dex):

1. Register Team Memory at the IdP as a confidential client with the redirect URI `https://memory.example.com/oauth/callback/<provider-id>`.
2. Write an SSO configuration file (format and Kubernetes Secret layout: [identity provider decision](docs/decisions/identity-provider.md); step-by-step: [authentication](docs/operations/authentication.md)).
3. Start the server with the public URL and the file:

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

Things to know:

- If SSO is configured but its configuration or secrets are missing or invalid, `/readyz` reports not ready and every request gets 503 until it is fixed.
- A first SSO sign-in creates a principal with **no roles**. Grant roles with `member add`, or map IdP groups to roles with `idp-groups add` and `"sync_group_roles": true` on the provider.
- To try SSO locally with disposable Keycloak and Dex, see [`deploy/dev-idp/`](deploy/dev-idp/README.md).

## Operator CLI: scopes, members and tokens

These commands connect as the operator role (`DATABASE_URL`), are trusted by virtue of that access, and are audited. Role changes are CLI-only for now: the portal's *Members* page is read-only. Run `./bin/team-memory help` for the full command list.

```bash
./bin/team-memory scope create --parent acme --level team --slug platform
./bin/team-memory scope create --parent acme/platform --level project --slug payments-api
./bin/team-memory principal create --name "Second Person"           # prints the principal id
./bin/team-memory member add --scope acme/platform --principal <id> --role writer --cascade
./bin/team-memory token create --principal <id> --ttl 720h           # prints the token once
./bin/team-memory member revoke --scope acme/platform --principal <id>
./bin/team-memory token revoke <token-id>
./bin/team-memory principal disable <id>
./bin/team-memory memory archive --id <memory> --as <maintainer> --expected-state-version <n>
./bin/team-memory audit list --scope acme/platform
```

Roles are `reader`, `writer` and `maintainer`. A grant applies to that scope only, unless `--cascade` extends it to all descendants. Anyone who can read a scope also sees its ancestors' memories, read-only.

`principal identities`, `link-identity` and `unlink-identity` are for operator recovery only. They attach or detach an upstream identity given as the exact `iss` and `sub`, never an email. Linking refuses an identity that is already linked to someone else; nothing ever moves an identity between principals.

## MCP surface

| Tool | Minimum role on the owning scope |
| --- | --- |
| `memory_search`, `memory_get`, `memory_history`, `memory_recent` | reader |
| `memory_create`, `memory_update` | writer |
| `memory_supersede`, `memory_approve` | maintainer |

`memory_search` takes `query`, `scope` (alias `project`), `types`, `limit`, `include_superseded` and `include_overridden`. Each result carries `origin_scope`, `inherited`, `precedence` (scope depth), `trust`, `approved_by`/`approved_at`, `revision`, `source_uri`, `updated_at`, a 0–1 rank-based `score` (not a calibrated probability), and the `retrievers` that matched it. `meta` reports whether semantic matching ran ([retrieval scoring](docs/decisions/retrieval-scoring.md)).

`memory_approve` only *requests* review of an exact revision (idempotent); it never changes trust. Approval, rejection and promotion happen only through explicit review in the portal.

Resources: `memory://scopes/{scope-path}/{overview|decisions|conventions|recent}`, with a `memory://projects/...` alias.

Treat retrieved memory content as untrusted data: people and agents write it, and it may contain instructions. Every result that carries memory content starts with an `UNTRUSTED DATA` notice.

## Deploying

Kubernetes with Helm:

1. [Authentication](docs/operations/authentication.md): Secrets to create, order of operations, solo and SSO mode.
2. [Deploy](docs/operations/deploy.md): topology, sizing, PostgreSQL choices, install steps.
3. [Helm chart README](deploy/helm/team-memory/README.md): required values and components.

Runbooks in [`docs/operations/`](docs/operations/): [upgrade](docs/operations/upgrade.md) (staged rollout, migrations, rollback), [backup and restore](docs/operations/backup-restore.md), [rotate secrets](docs/operations/rotate-secrets.md), [dead letters](docs/operations/dead-letters.md), [incident](docs/operations/incident.md), [secret purge](docs/operations/secret-purge.md), [mirror](docs/operations/mirror.md), [observability](docs/operations/observability.md). Measured numbers: [load test](docs/operations/load-test-report.md), [restore drill](docs/operations/restore-drill-report.md).

## Development

```bash
make test
```

`make test` starts the local database and runs unit and integration tests against real PostgreSQL with the race detector. Each integration test creates and drops its own database. A plain `go test ./...` without `TEST_DATABASE_URL` skips the database tests. Run `make help` for every target.

| Target | What it does |
| --- | --- |
| `make test-unit`, `make test-security` | unit tests only (no database); adversarial security suite only |
| `make test-e2e` | portal browser tests in Chromium (first: `cd e2e && npm ci && npx playwright install chromium`) |
| `make lint` | `go vet` and golangci-lint |
| `make generate`, `make check-generate` | regenerate sqlc code after changing `internal/postgres/queries/`; fail if it is stale |
| `make run`, `make worker`, `make dev-roles` | run the server or worker locally; create local login roles |
| `make eval`, `make bench-vector` | retrieval evaluation against a real model ([report](docs/eval/retrieval-report.md)); vector index benchmark |
| `make image`, `make helm-lint`, `make check-alerts`, `make secret-scan` | container image; chart lint; alert-rule tests; gitleaks |
| `make dev-idp-up`, `make dev-git-up`, `make test-mirror-remote` | disposable Keycloak + Dex; disposable Gitea and authenticated mirror tests |
| `make k3d-up`, `make k3d-down`, `make rollback-drill`, `make backup-drill`, `make loadtest` | disposable cluster, drills, load-test tool |

Layout:

| Path | Contents |
| --- | --- |
| `cmd/team-memory` | the single binary: server, workers and operator CLI |
| `internal/memory` | memory engine (no transport imports) |
| `internal/search`, `internal/embeddings`, `internal/jobs` | hybrid retrieval, embedding providers and jobs, queue and worker |
| `internal/mcp`, `internal/server`, `internal/portal` | MCP adapter, HTTP assembly and readiness, web portal |
| `internal/auth`, `internal/auth/oauth` | tokens, bootstrap, identity mapping; OAuth authorization server |
| `internal/mirror`, `internal/telemetry` | git export, verify, rebuild; metrics and tracing |
| `internal/postgres`, `migrations/` | pool, migrations, transactions, sqlc code; SQL migrations |
| `integration/`, `e2e/`, `loadtest/` | integration and security tests, browser tests, load-test tool |
| `deploy/` | Helm chart, backups, dev environments (IdP, Gitea, k3d) |

See [CONTRIBUTING.md](CONTRIBUTING.md) for the rules CI and reviewers enforce.

## Documentation map

| Area | Where |
| --- | --- |
| Using Team Memory | [portal guide](docs/portal.md), [Claude Code](docs/clients/claude-code.md), [Codex](docs/clients/codex.md), [agent policies](docs/agent-policy/) |
| Running it | [local Docker](docs/operations/local-docker.md), [`docs/operations/`](docs/operations/), [Helm chart](deploy/helm/team-memory/README.md) |
| Security | [threat model](docs/security/threat-model.md), [limits](docs/security/limits.md), [SECURITY.md](SECURITY.md) |
| Design decisions | [`docs/decisions/`](docs/decisions/) (RLS, scopes, retrieval, identity, portal, mirror, …) |
| Evidence and history | [`docs/verification/`](docs/verification/), [`docs/eval/`](docs/eval/), [`docs/handoff/`](docs/handoff/) (one per build stage), [release checklist](docs/release-checklist.md) |

## Status and limitations

- Personal proof of concept, maintained by one person, with no support commitment. No tagged version yet. Versioning policy: [CONTRIBUTING.md](CONTRIBUTING.md#versioning-policy).
- Not yet used by more than one real person. The only multi-identity evidence uses synthetic test identities.
- Embeddings need a reachable Ollama; without one, search is full-text only.
- Git mirrors are operator-only. Users browse history through the service.
- Measured gaps against the performance targets are listed in the [load-test report](docs/operations/load-test-report.md).
- No external security review has been done, and none is planned. See [SECURITY.md](SECURITY.md) and the [threat model](docs/security/threat-model.md) for residual risks.

## Contributing and security

[CONTRIBUTING.md](CONTRIBUTING.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), [SECURITY.md](SECURITY.md), [CHANGELOG.md](CHANGELOG.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
