# Team Memory

A self-hosted, permission-aware shared memory for developers and their AI coding agents, served over MCP (Model Context Protocol).

> **Status: early development (stage 01 of 7).** Not production-ready. There is no OAuth/SSO, no row-level security, no approval workflow, and no web portal yet. See `proposal.md` for the plan and `docs/handoff/` for progress.

This repository holds both the service code and the planning material (`proposal.md`, `review-decisions.md`, `prompts/`).

## What works today

- Versioned memories (`fact`, `decision`, `convention`, `lesson`, `context`) with immutable content revisions, immutable lifecycle events, optimistic concurrency, supersession and archival.
- Hierarchical scopes (organization → team → project, configurable levels and depth). Memories are inherited downward only and labeled with `origin_scope` and `inherited`.
- PostgreSQL full-text search.
- MCP over Streamable HTTP with eight tools and four resource views. Serves protocol 2026-07-28 and older clients that use the `initialize` handshake.
- Personal access tokens: shown once, stored hashed, with expiry and revocation. There is no unauthenticated mode.

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

```bash
TEAM_MEMORY_DEFAULT_SCOPE=acme/default ./bin/team-memory serve
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

## Scopes, members and tokens (operator CLI)

These commands act directly on the database. Authorized administration through the API arrives in stage 04.

```bash
./bin/team-memory scope create --parent acme --level team --slug platform
./bin/team-memory scope create --parent acme/platform --level project --slug payments-api
./bin/team-memory principal create --name "Second Person"
./bin/team-memory member add --scope acme/platform --principal <id> --role writer --cascade
./bin/team-memory token create --principal <id> --ttl 720h
./bin/team-memory token revoke <token-id>
```

Grants on a scope apply to that scope only, unless `--cascade` extends them to all descendants. Anyone who can read a scope also sees its ancestors' memories, read-only.

## MCP surface

Tools: `memory_search`, `memory_get`, `memory_create`, `memory_update`, `memory_supersede`, `memory_history`, `memory_approve`, `memory_recent`. `memory_approve` only ever *requests* review. It is not implemented until stage 04, and approval itself will be a browser-only action.

Resources: `memory://scopes/{scope-path}/{overview|decisions|conventions|recent}`, with a `memory://projects/...` alias.

Treat retrieved memory content as untrusted data: it is written by people and agents and may contain instructions.

## Development

```bash
make test
```

`make test` runs unit tests and integration tests against real PostgreSQL. Each integration test creates and drops its own database.

Other targets: `make generate` (sqlc), `make check-generate`, `make lint`, `make run`.

Layout: `cmd/team-memory` (single binary), `internal/memory` (engine, no transport imports), `internal/mcp` (MCP adapter), `internal/auth` (tokens, bootstrap), `internal/postgres` (pool, migrations, transactions, sqlc code), `migrations/`, `integration/`, `docs/`.

## License

Apache License 2.0. See `LICENSE`.
