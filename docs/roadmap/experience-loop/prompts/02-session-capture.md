# 02: Session capture

## Objective

Let Claude Code and Codex upload agent sessions to a Team Memory scope. Capture must be opt-in, redacted, retention-limited, isolated by RLS and audited. It gives the curator raw material without making Team Memory a store of unprotected transcripts.

## Context

Team Memory is a self-hosted Go service (PostgreSQL 17, pgvector, sqlc, forced RLS, scoped memories, approval only in the portal, an append-only audit log). This stage adds the first piece of the Stash-style experience loop. The design is fixed by stage 01's decisions. Do not change those decisions here; if one proves unworkable, stop and report it.

## Inputs

- `docs/handoff/experience-loop-01.md` and the decision records it names (session capture, transcript retention). If they are missing, stop and ask for them.
- `docs/security/threat-model.md`, `docs/decisions/rls.md`, `docs/decisions/client-attribution.md`
- Existing code: `internal/auth/`, `internal/secrets/`, `internal/jobs/`, `internal/server/`, `internal/postgres/queries/`, `migrations/`, `integration/security/`

## Work

1. **Migrations.** Add forward-only migrations for the session tables (for example sessions, session events or turns, and redaction reports), following stage 01's data model.
   - Each table has a scope column, with RLS enabled and forced.
   - Grant only the privileges the app and worker roles need.
   - Add per-scope capture settings: enabled, retention, size limits.
2. **Ingest endpoint.** Add an authenticated HTTP endpoint for uploading sessions. It is separate from the MCP tool surface unless stage 01 decided otherwise.
   - Reuse PAT and OAuth authentication, rate limits and request size limits.
   - Reject uploads to scopes where capture is disabled or the caller lacks write access. Give the same response in both cases, so it does not reveal whether a scope exists.
   - Record client attribution.
3. **Redaction.** Run the secret scanner and any additional patterns before anything is written. Never store the unredacted content, including in logs, traces and error messages. Store a redaction report only if stage 01 decided to.
4. **Retention.** Add a purge job (an outbox job, or part of the existing worker) that deletes expired sessions and writes audit events. Add an operator command to purge a scope or a single session on demand.
5. **Audit.** Add audit events for: capture enabled or disabled, session ingested, session read in the portal (if reading is allowed), and session purged.
6. **Client hooks.** Add hook scripts or configuration for Claude Code and Codex that upload a session at its end.
   - Put them under `docs/clients/` or `deploy/hooks/`.
   - Read the token from an environment variable and never write it to disk.
   - The hooks must fail open: a failed upload never blocks the agent.
   - Document how to pause capture.
7. **Portal.** Add a scope settings page where an owner can enable capture and set retention. Add a session list and detail view only if stage 01 allows reading transcripts.
8. **Tests.**
   - Unit tests for redaction.
   - Integration tests for ingest, limits and purge.
   - Adversarial tests in `integration/security/`: cross-scope reads and writes, uploads to a disabled scope, oversized and malformed payloads, secrets in every field, a caller without write access, and checks that RLS holds when application checks are bypassed.

Excluded: curation, embeddings of sessions (unless stage 01 requires them), skills, and the CLI.

## Deliverables

- Migrations, regenerated sqlc output, ingest and purge code, hook files, portal settings and tests.
- Documentation: `docs/operations/session-capture.md`, covering enabling capture, retention, purge, hook setup and what is stored. Add a README section.
- A `CHANGELOG.md` entry.
- `docs/handoff/experience-loop-02.md`.

## Acceptance criteria

- `make test`, `make test-security` and `make lint` pass.
- A synthetic Claude Code session and a synthetic Codex session, both containing seeded fake secrets, are ingested into an enabled scope with every secret redacted. An upload to a disabled scope is rejected.
- A user in a sibling scope cannot read or list the session through any API, portal page or direct SQL as the app role.
- An expired session is removed by the purge job, and the purge appears in the audit log.
- The hooks do not block the client when the server is unreachable.

## Handoff

Record in `docs/handoff/experience-loop-02.md`:

- The table and endpoint names.
- How the curator should read new sessions: queries, watermark fields, and which role it uses.
- Redaction coverage and its known gaps.
- Hook installation status for each client.
- Open issues.
