# 06: standards-api integration

## Objective

Connect Team Memory and standards-api so an approved convention can become a code-review rule, or a standard can feed Team Memory, following the contract chosen in stage 01. The two projects stay separate. Team Memory governs knowledge, and standards-api serves review-time rules filtered by repository, language and changed paths.

## Context

- **Team Memory** is a self-hosted Go service. It holds versioned, scoped memories with forced RLS and approval only in the portal.
- **standards-api** is a TypeScript service (Node.js 22, Fastify, Prisma, PostgreSQL, Vitest) with:
  - A `Standard` model: `ruleKey`, `version`, `status`, `severity`, `category`, `appliesTo`, `ruleText`, `reviewGuidance`, examples, `rationale`, `tags`, `sourceUrl`, `owner`, `approvedBy`.
  - REST endpoints `GET /api/v1/standards`, `/latest`, `/applicable` and `/:ruleKey`, plus `POST` and `PUT` writes.
  - MCP tools `list_standards`, `get_standard`, `latest_standards` and `applicable_standards`.
  - A single shared write key, `STANDARDS_API_KEY`.

Approval should stay in one place. If Team Memory is the source, standards-api must not accept edits to synced fields that bypass Team Memory.

## Inputs

- `docs/handoff/experience-loop-01.md` and `docs/decisions/standards-api-contract.md`: the sync direction, the field mapping and the credential decision. If either is missing, stop and ask for it.
- The standards-api repository, including its `README.md`, `prisma/schema.prisma`, `src/` and tests. Its own `prompts/` folder shows how it has been built in stages so far.
- Team Memory's `internal/memory/` lifecycle events and `internal/mirror/` (an example of an event-driven exporter with its own worker).

## Work

Implement only the direction stage 01 chose. For direction (a), where Team Memory pushes approved conventions to standards-api:

1. **Changes in standards-api, done first in its own branch and PR:**
   - If stage 01 required it, replace or extend the single write key with per-client credentials, so the Team Memory sync can be identified and limited.
   - Add a source marker on standards (for example `source = team-memory` and `source_ref`), with a Prisma migration.
   - Reject manual edits to synced standards, or flag them as drift.
   - Add Vitest coverage.
2. **Mapping in Team Memory.** Store the standards metadata that stage 01 mapped (severity, category, `appliesTo`, review guidance, examples) on `convention` memories in the way stage 01 chose. Edit it in the portal and approve it as part of the revision.
3. **Sync worker in Team Memory.**
   - An outbox-driven exporter in the style of `internal/mirror`. On approval, supersession or archival of an opted-in convention, it creates, versions or deprecates the matching standard.
   - It must be idempotent, generate `ruleKey` deterministically, retry with backoff and dead-letter failures.
   - It records `approvedBy` from the Team Memory approver.
   - Each scope opts in, set by an operator.
4. **Drift report.** Add an operator command (and optionally a portal view) that compares synced standards with their source revisions.

For direction (b) or (c), adapt the steps to match the contract. Imported standards must arrive in Team Memory as approved, read-only memories whose provenance points to standards-api, and must never be editable in Team Memory without a new approval.

Tests:

- Contract tests in both repositories against a shared fixture of mapping examples.
- An end-to-end test in Team Memory against a disposable standards-api instance, if one can be started in tests.
- Adversarial tests: an unapproved revision never syncs; a convention from a scope that has not opted in never syncs; credentials are never logged; a malformed `appliesTo` value is rejected on both sides.

Excluded: merging the two codebases, and changing standards-api's read API in ways that break its current ReviewOps clients.

## Deliverables

- A standards-api branch or PR with its changes, tests and README updates. Do not merge or push without the author's approval.
- Team Memory's mapping fields, sync worker, drift command, tests, `docs/operations/standards-sync.md` and a `CHANGELOG.md` entry.
- `docs/handoff/experience-loop-06.md`.

## Acceptance criteria

- `make test`, `make test-security` and `make lint` pass in Team Memory. `npm run lint` and `npm test` pass in standards-api.
- One approved convention with `appliesTo.changed_paths` set reaches standards-api. `applicable_standards` returns it for a matching path and does not return it for a path that does not match.
- Superseding the convention in Team Memory creates a new standard version. Archiving it deprecates the standard.
- The drift report detects a manual change made directly in standards-api.

## Handoff

Record in `docs/handoff/experience-loop-06.md`:

- The sync direction as implemented.
- The credentials setup, without any values.
- The mapping table as built.
- The standards-api PR link.
- Open issues.
