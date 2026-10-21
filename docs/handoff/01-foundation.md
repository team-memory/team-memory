# Handoff: stage 01 (foundation)

Date: 2026-10-20. Repository: `/Users/tyler/Documents/projects/memory/team-memory/` (service code and planning files share one repository, at the author's request; the prompts were updated to match).

## Summary

The Go service, schema, memory engine, PAT authentication, MCP adapter, tests, CI workflow and stage documents are in place. Every stage 01 acceptance criterion was checked; results and caveats are below. A local `go test` run passed. The CI workflow has **not** run yet, because nothing has been pushed.

## Acceptance criteria

| Criterion | Evidence | Result |
| --- | --- | --- |
| `make test` passes, including integration tests against real Postgres | `make test` (`go test -race -count=1 ./...`), 12 integration tests + 7 unit tests, PostgreSQL 17 + pgvector 0.8.1 | Pass |
| MCP client: initialize, list tools, create, update, stale-revision conflict, keyword search, history with both revisions | `integration/mcp_test.go` (official go-sdk client, both protocol 2025-11-25 `initialize` and 2026-07-28 `server/discover`); real clients also exercised every step across two runs (create/search/get on 2026-10-20; update, stale-revision conflict and history on 2026-10-21) | Pass (see notes 1 and 2) |
| Revision rows cannot be modified or deleted | `TestRevisionsImmutable`: UPDATE, DELETE and TRUNCATE rejected by triggers; the same test covers events, audit, overrides, approvals, the closure table and scopes | Pass |
| Scope-tree tests (three levels + sibling project) | `TestScopeInheritance`, `TestInheritedMemoriesReadOnlyFromDescendants`, `TestTreeRules`, `TestOverridesAreModeledButDoNotHide` | Pass |
| Token after `init` works; missing/invalid/revoked/expired tokens rejected; no unauthenticated mode | `TestInitAndTokens`, `TestMCPRequiresAuthentication`; init rerun refuses and keeps the owner | Pass |
| `internal/memory` does not import `internal/mcp` | `internal/memory/imports_test.go` (`go list -deps`; also forbids the MCP SDK) | Pass |
| Early PAT cross-client smoke test, both directions | `docs/verification/early-clients.md` | Pass (see note 2) |
| Landscape findings cited, no unverified superiority claims | `docs/landscape.md` | Done |
| CI includes foundational build/test checks | `.github/workflows/ci.yml` (build, vet, sqlc consistency, race tests with pgvector service) | Written, **not yet run** |

Notes:

1. The spec changed since the prompt was written. MCP 2026-07-28 removed `initialize`. "Initialize" is satisfied by the legacy handshake for older clients and `server/discover` for current ones, and both are tested.
2. The smoke test passed in both directions. On 2026-10-21, a re-verification covered the steps the first run missed. Claude Code ran update, the stale-revision conflict, history and a resource read. Codex then read the history, the resource and `memory_recent`. Create and keyword search rely on the 2026-10-20 run. The earlier missing Claude Code `tools/call` came from the test command's `--allowedTools` list, not from the server (details in the verification note).

## Schema changes vs the draft, and why

- **Scope tree replaces organizations/projects.** `scopes` (adjacency + slug path) and `scope_closure`, with triggers deriving depth/path/org, enforcing max depth, allowed levels and organization-only roots, and forbidding moves and deletes. `org_settings` holds levels, max depth and the owner. `scope_members` (role + `cascade`) replaces `project_members`. Rationale: `docs/decisions/scope-tree.md`.
- **Principals keep identity mapping** in a separate `principal_identities (issuer, subject)` table, so explicit SSO linking (stage 03–05) can attach several identities to one principal.
- **Current state vs history.** `memories` is a mutable projection (status, trust, current revision, `state_version`). `memory_revisions` holds immutable content snapshots (title, content, rationale, source_uri, kind). `revision_overrides` holds override links as part of the revision. `memory_events` holds immutable lifecycle events with a JSON snapshot, so stage 06 can export a delayed event without reading current state. `approvals` are immutable revision-bound outcomes.
- **Title added.** Revisions have a required `title` (max 200 characters) for listings and resources. The draft had no title.
- **Tenant columns everywhere.** `org_id`/`scope_id` are on revisions, events, embeddings, overrides and approvals, with composite FKs to `scopes (id, org_id)`, so stage 04 RLS policies need no joins.
- **Shapes for later stages:** `promotion_requests` (with a snapshot column for narrow destination visibility), `audit_events` (append-only), `idempotency_keys`, `jobs` (multi-kind outbox: `embed`, `git_mirror`), `mirror_state` (per event × repository), `memory_embeddings` (`vector(768)`).
- **Append-only enforcement** by row triggers (UPDATE/DELETE) and statement triggers (TRUNCATE) on revisions, overrides, events, approvals, audit and the closure table; scopes reject DELETE, TRUNCATE and structural UPDATE. Triggers do not stop a superuser or table owner from disabling them; the local dev role is a superuser. Stage 04 must run the app as a non-owner role.
- Migrations are forward-only. Down raises an error. Tool: goose (`docs/decisions/migrations-tool.md`).

## Other decisions

- **MCP SDK:** official go-sdk v1.8.0, stateless Streamable HTTP, JSON responses, bearer middleware. Reasons and client protocol versions are in `docs/decisions/mcp-sdk.md`.
- **Single binary** `cmd/team-memory` with subcommands (`serve`, `migrate`, `init`, `scope`, `principal`, `member`, `token`), replacing the draft `cmd/{server,worker,migrate}`. A worker is not needed until stage 02 and can be a `worker` subcommand.
- **Transaction helper** `internal/postgres/transactions.go` sets `app.principal_id` with `set_config(..., true)`, which is transaction-local like `SET LOCAL` but takes a bind parameter. Operator CLI paths use the nil UUID.
- **Not found hides existence.** Unreadable scopes and memories return `not_found`. `permission_denied` is returned only when the target is visible.
- **Trust:** `cited` when a syntactically valid http(s) `source_uri` without credentials is present, else `unverified`. Every new revision resets trust. Nothing sets `approved` yet.
- **Search rank** is `ts_rank_cd`, labeled in results as ordering-only, not a calibrated score. Stage 02 defines the documented relevance score.
- **Module path** `github.com/team-memory/team-memory` is a placeholder. Change it before the public release if the GitHub location differs.
- `TEAM_MEMORY_DEFAULT_SCOPE` lets clients omit `scope`. `project` is accepted as an alias.

## Review fixes (same session)

A read-only review subagent checked the stage 01 changes. Its findings were confirmed and fixed:

- TRUNCATE was not blocked on `revision_overrides`, `approvals`, `scope_closure` and `scopes`. Statement triggers were added, and tests now cover them.
- `memory_update`/`memory_supersede` with a `scope` argument revealed whether any scope path existed (`inherited_read_only` vs `not_found`). `inherited_read_only` is now returned only when the memory really is inherited into a scope the caller can read; anything else is `not_found`. Regression test added.
- The memory row was locked before the access check, so any principal could lock invisible memories. The check now runs first.
- `ConfigureOrg` could race with scope creation. It now locks `org_settings` FOR UPDATE, and the scope trigger reads settings FOR SHARE. This one is not covered by a concurrency test.
- The supersede `reason` was lost; the original memory's `superseded` event now records it with the replacement id. Tested.

These were fixed by amending `00001_foundation.sql`, which is acceptable only because nothing has been committed or deployed. The local dev database was migrated with the earlier version, so run `make db-down && make db-up` to pick up the new triggers.

## How to run

See `README.md`. In short: `make db-up`, `make build`, `./bin/team-memory init --org acme --owner "Name"`, then `./bin/team-memory serve`, and `make test`.

The local dev database currently holds the smoke-test organization `acme` (project `acme/smoke`). `make db-down` deletes it.

## Skipped or not verified

- The CI workflow has not run (no push yet). Action versions `actions/checkout@v5` and `actions/setup-go@v6` are assumed to be current.
- Real clients have not exercised `memory_supersede` or `memory_approve`. The SDK protocol test covers them. Every other tool and one resource were exercised by at least one real client. `memory_update` was run only by Claude Code.
- The request log now records the JSON-RPC method for clients that send no `Mcp-Method` header (Codex on 2025-06-18). It reads the method from the body after authentication, without reading ahead. Before this fix, those requests were logged with an empty method. Requests rejected before their body is read still log an empty method.
- Pre-existing, not fixed: the HTTP server sets only `ReadHeaderTimeout`, with no `ReadTimeout` or `IdleTimeout`, so a slow client can hold a connection open. The `Mcp-Method` and `User-Agent` headers are logged with no length cap. Both are candidates for stage 06 hardening.
- No secret scanning yet. The proposal requires it before export and promotion. Not in the stage 01 list, so it is an open item for stage 04.
- No Helm chart, metrics, tracing or worker (later stages, per the prompt's exclusions).
- `audit_events` gets rows for memory and membership mutations, but reads, denials and authentication failures are not audited yet.

## Open issues

For stage 02 (retrieval):
- `memory_embeddings.embedding` is fixed at `vector(768)`. Decide how model or dimension changes work without rewriting knowledge records (for example a per-model table or partial indexes per dimension).
- Define the honest relevance score and evaluation set. Search currently returns only active memories of current revisions.
- Index strategy for filtered vector search over `scope_id = ANY(visible scopes)`.

For stage 03 (identity):
- Claude Code 2.1.292 uses MCP 2026-07-28 and Codex 0.163.0 uses 2025-06-18, so the OAuth flow must be tested with both. 2026-07-28 deprecates DCR in favor of Client ID Metadata Documents.
- `principal_identities` is ready for issuer/subject mapping. CLI owner bootstrap is decided and implemented; do not reopen it.

For stage 04 (security):
- RLS policies over the closure table: the helper queries `RolesOnScope`, `ReadableScopes` and `CanReadMemoriesOf` express the rules and should be mirrored in policies. Rows already carry `org_id`/`scope_id`.
- Implement `Service.RequestReview` (currently `ErrNotImplemented`), approval-gated overrides (links are recorded and reported as `override_status: proposed`), promotions, rejection, authorized membership administration and audit of denials.
- `Principal.AuthMethod` is set to `pat` so approval can be kept out of reach of token-based calls.
- Archive is a service method (maintainer only) with no MCP tool, keeping the eight-tool contract.
- Secret scanning before writes, promotion and export.

## Re-estimate

Stage 01 took one working session, including the repository move. The largest unknowns for the remaining stages are OAuth interoperability across two clients on different protocol generations (stage 03), and RLS over the closure table with adversarial tests (stage 04). There is no measured basis yet for a calendar estimate.
