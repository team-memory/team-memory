# Stage 01: Foundation

## Objective
Create the Team Memory Go repository with migrations, memory lifecycle services with immutable content revisions and lifecycle events, a real MCP server over Streamable HTTP, basic full-text search, and a local dev environment — so that a memory can be created, updated, searched, and read through an MCP client.

## Context
Team Memory is an open-source (Apache 2.0), self-hosted Go service giving AI coding agents (Claude Code, Codex, any MCP client) a shared, versioned, permission-aware knowledge base on PostgreSQL 17+ with pgvector. Memories have a kind (`fact`, `decision`, `convention`, `lesson`, `context`), a status (`active`, `superseded`, `archived`), and a trust level (`unverified`, `cited`, `approved`). Tenancy is a scope tree, defaulting to organization → team → project, with memories owned by a scope. Architecture: modular monolith; `internal/memory` knows nothing about MCP. Full scope is in `proposal.md`. This is the first of seven stages; the authentication architecture is confirmed in stage 03.

## Inputs
- `prompts/00-start-here.md` (attach; follow its global rules and completion gates).
- `review-decisions.md` (attach with the proposal; required accepted security/lifecycle decisions).
- `proposal.md` (attach). If you cannot see it, ask the user for it.
- The draft schema and repo layout below.

Draft layout: `cmd/{server,worker,migrate}`, `internal/{auth,mcp,memory,search,embeddings,postgres,jobs,audit}`, `migrations/`, `deploy/helm/team-memory/`, `integration/`, `Dockerfile`, `README.md`.

Draft core tables: `organizations`, `projects` (unique `(org, name)`, unique `(id, org)`), `principals` (`issuer`, `subject`), `project_members` (role `reader|writer|maintainer`) — **note:** organizations/projects and project membership are replaced by the scope tree in Work step 3; retain principals and their identity mapping, `memories`, `memory_revisions` (content, rationale, source_uri, changed_by, generated `tsvector`), `memory_embeddings` (memory_id, revision, model, `vector(768)`).

## Work
0. Before implementation, perform a focused source-backed review of existing shared MCP memory projects and repository-based workflows. Record overlap, gaps, and limits in `docs/landscape.md` once the repo exists. Do not claim novelty without evidence or change the agreed stack/scope unilaterally. Record implications for the pilot.

1. Set up the service in the existing repository root the repository root, alongside `prompts/` and the planning files (do not create a separate repository). Go module, Apache 2.0 `LICENSE`, `.gitignore`.
2. Local dev environment: docker compose (or equivalent) with PostgreSQL 17 + pgvector; a `Makefile` or task runner for migrate, test, run, sqlc generate.
3. Migrations (choose and document a migration tool). Start from the draft schema but fix known gaps rather than copying blindly:
   - **Replace the fixed organization/project pair with a scope tree.** One `scopes` table (id, parent_id, org root id, level label such as `organization`, `team`, `project` or an admin-defined name, slug, depth, materialized path or closure table; choose and justify in `docs/decisions/scope-tree.md`), with a configurable maximum depth (default 5) and a rule that every scope has exactly one parent except the org root. `memories.scope_id` points at the scope that owns the memory. `scope_members` (scope, principal, role `reader|writer|maintainer`, `cascade` boolean) replaces `project_members`. Semantics, to implement in the memory service and later enforce in RLS (stage 04): (a) a principal can **read a scope's own memories** with a membership there or a `cascade=true` grant on an ancestor; (b) anyone who can read a scope can also read the memories of all its **ancestors** (inherited, read-only, never copied); (c) **nothing flows up**: reading a scope never reveals descendants' memories; (d) writing, superseding, and approving a scope's memories needs the matching role on that scope or a cascading grant on an ancestor; (e) an inherited memory can only be changed from its owning scope. Expose a service method that, for a principal and scope, returns the visible scope set (the scope itself plus ancestors) and a separate one for "scopes I can browse into", used by search, resources, and the portal. Moving a scope to a new parent is out of scope for v1.
     Model override links in immutable content revisions and promotion provenance now; implement their approval-dependent behavior in stage 04 according to `review-decisions.md`. Drafting an override never hides inherited guidance. Promotion requires source consent and destination browser acceptance, with narrow request-snapshot visibility and atomic source supersession/destination creation. Stage 01 does not finalize approvals or promotions.
   - Add immutable lifecycle-event records and a separate current-state/version projection as needed. Mirror idempotency is event-based, not content-revision-based; approvals and status changes must not mutate content revisions.
   - Add scope creation/listing/configuration CLI and service methods, plus initial membership bootstrap. Define archive/supersede contracts; complete authorized membership administration and review transitions in stage 04.
   - `memory_revisions` and `memory_embeddings` need tenant columns (or a reliable join path) so RLS can protect them directly.
   - Add organization-level settings (allowed levels and max depth), an append-only `audit_events` table, `approvals`, `idempotency_keys`, and a durable `jobs` (outbox) table shape. Full behavior for audit/approval/RLS lands in stage 04, but design the tables now so later stages do not need destructive changes.
   - Design the `jobs` table to hold more than one job kind (`embed`, `git_mirror`) and add a `mirror_state` shape (per event and repository: commit SHA, delivery state, exported at) so stage 06 can add the git mirror without a destructive migration. Do not implement the mirror here.
   - Enforce revision immutability (no UPDATE/DELETE on revisions).
4. Use `pgx` and `sqlc`. All DB access goes through transactions that run `SET LOCAL app.principal_id` (even though RLS policies are enabled in stage 04, build the transaction helper now in `internal/postgres/transactions.go`).
5. `internal/memory`: `Service` with Create, Update (with `expectedRevision` optimistic concurrency, returning a typed conflict error), Get, History, Recent, Supersede, and full-text Search. Validation: size limits, allowed kinds, URI shape for `source_uri`. No MCP imports.
6. `internal/auth`: define an `Authenticator` interface producing a `memory.Principal`. Implement **personal access tokens** as the first real authenticator: random tokens shown once, stored hashed, with expiry and revocation, mapped to a principal. Add `team-memory init` (in `cmd/`) that creates the org, a default project, and an admin principal with a token, so a single person can run the whole system with no IdP. Do not add an unauthenticated or "dev bypass" mode. Upstream OIDC/SSO and the OAuth login flow come in stages 03–04; keep the interface open for them.
7. `internal/mcp`: implement MCP over Streamable HTTP using a Go MCP SDK. First verify against the current official MCP specification and SDK which SDK is maintained and supports Streamable HTTP, tools, resources, and the authorization features needed later; record the choice in `docs/decisions/mcp-sdk.md` with reasons and alternatives. Implement JSON-RPC initialization, capability negotiation, tool listing and calls, and structured errors for the 8 tools: `memory_search`, `memory_get`, `memory_create`, `memory_update`, `memory_supersede`, `memory_history`, `memory_approve` (stub returns a clear "not implemented until stage 04" error), `memory_recent`. Add the four resources: `memory://scopes/{scope-path}/{overview|decisions|conventions|recent}` (a scope path such as `acme/platform/payments-api`; keep `memory://projects/...` as an alias for leaf scopes if useful). Tool inputs accept `scope` (path or id); `project` is accepted as an alias. Search, `memory_recent`, and the resources return the scope's own memories **and** inherited ancestor memories, each result carrying `origin_scope` and an `inherited: true|false` flag; they never include descendants.
8. Tests: unit tests for lifecycle and concurrency conflicts; integration tests against real PostgreSQL (not mocks) in `integration/`; an MCP protocol test that performs initialize → tools/list → tool call using a real client or the SDK client.
9. `README.md` with quickstart; keep hostnames as `memory.example.com`. Add initial CI for Go build, unit/integration tests with PostgreSQL, and sqlc generation consistency; extend it in later stages.
10. Run an early basic cross-client smoke test using the installed Claude Code and Codex versions: create with one and read with the other using PAT authentication. Record commands/versions and observed results in `docs/verification/early-clients.md`; do not wait until stage 07 to discover basic incompatibility. Missing clients or failed required checks leave this gate incomplete; continue independent work and record the blocker.

## Excluded
Embeddings, vector search, the git mirror, OIDC, RLS policies, Helm, approval workflow behavior, any UI.

## Deliverables
- Working repo with migrations, server, tests, compose file, README.
- `docs/decisions/mcp-sdk.md`, `docs/decisions/migrations-tool.md`, `docs/decisions/scope-tree.md`, `docs/landscape.md`, `docs/verification/early-clients.md`, initial CI workflow.
- `docs/handoff/01-foundation.md`.

## Acceptance criteria
- `make test` (or equivalent) passes, including integration tests against real Postgres.
- Using an MCP client against the running server: initialize, list tools, create a memory, update it with the correct expected revision, see a conflict error with a stale revision, search it by keyword, and fetch its history showing both revisions.
- Revision rows cannot be modified or deleted (proved by a test).
- Scope-tree tests at the service level (before RLS exists) with a three-level tree and a sibling project: a project reader sees project, team, and org memories with correct `origin_scope` and `inherited` flags; a team reader never sees project memories; an org-only member never sees team or project memories; a sibling project never sees another project's memories; a non-cascading org grant does not confer access to descendants, while a cascading one does; max depth and single-parent rules are enforced; inherited memories cannot be edited from a descendant scope. Override drafts and promotion provenance are modeled; approval-dependent behavior and adversarial tests land in stage 04.
- After `team-memory init`, a client using the printed token can call tools; requests without a valid token are rejected; a revoked or expired token is rejected (tests). No unauthenticated mode exists.
- `internal/memory` has no import of `internal/mcp` (check with a test or lint rule).

- Early PAT cross-client smoke test passes with observed results for both installed clients. Landscape findings are cited and do not imply unverified superiority. CI includes the foundational build/test checks.

## Handoff
Write `docs/handoff/01-foundation.md`: schema changes made versus the draft and why, SDK choice, how to run everything, test results (including anything skipped), and open issues for stages 02 and 04.
