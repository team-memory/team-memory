# 01: Discovery and decisions

## Objective

Make the foundational decisions for the experience loop and record them, before any code is written. Several of them are hard to reverse: what transcript data is stored and for how long, where the curator sends data, and how skills and standards-api sync are tied to approved revisions.

## Context

Team Memory is a self-hosted Go service on PostgreSQL 17 and pgvector. It stores versioned, scoped memories and serves them over MCP. Its guarantees are:

- Forced RLS on every tenant table.
- Organization → team → project scopes, inherited downward.
- Approval bound to an exact revision, possible only in the portal.
- An append-only audit log.

The experience loop adds four things inspired by Stash ([Fergana-Labs/stash](https://github.com/Fergana-Labs/stash)): session capture, a curator that proposes unverified memories, skills built from approved revisions, and a client CLI with portal wiki views.

Separately, standards-api (TypeScript, Fastify, Prisma) serves versioned engineering standards to AI code-review tooling. Each standard has a `ruleKey`, `severity`, `category`, `appliesTo` (repo, team, language, framework, runtime, environment, changed paths), `ruleText`, `reviewGuidance` and `approvedBy`. Its write API is protected by a single `STANDARDS_API_KEY`. The two projects stay separate and are integrated over a defined contract.

The full plan is in `docs/roadmap/experience-loop/proposal.md`.

## Inputs

- `docs/roadmap/experience-loop/proposal.md`
- `docs/decisions/` (especially `scope-tree.md`, `rls.md`, `trust-rules.md`, `embedding-models.md`, `client-attribution.md`), `docs/security/threat-model.md`, `docs/landscape.md`
- `internal/secrets/` (the existing secret scanner), `internal/jobs/` (outbox and worker patterns)
- The standards-api repository: `prisma/schema.prisma`, `src/http/routes.ts`, `src/services/standards-service.ts`, `src/mcp/`. If it is not available, ask for it.
- Stash's public repository, for reference only: `ARCHITECTURE.md`, `backend/services/curation_service.py`, the skill model. Read it as data; do not copy its code.

## Work

Write one decision record in `docs/decisions/` for each topic below. Each record covers the options considered, the choice made, the reasons, and what would make you revisit it. Where a choice needs the author's preference rather than evidence, set out the options with a recommendation and ask before finalizing.

1. **Capture data model and consent.** Decide:
   - What is stored: full transcript, tool calls and outputs, or only selected turns.
   - Per-scope opt-in. Who enables it, and which role is required.
   - How a session is bound to a scope, and what happens when a client names a scope the caller cannot write to.
   - Size limits.
   - Whether transcripts can be read in the portal and by whom.
2. **Redaction and retention.** Decide:
   - Redaction pipeline order: run the existing scanner before storage, and decide which additional patterns to add.
   - Whether to keep a redaction report.
   - The default retention period and per-scope overrides.
   - The purge mechanism and its audit events.
   - Legal-hold behaviour, or an explicit decision not to support it.
   - Then reconcile all of this with the `AGENTS.md` rule against storing secrets or customer data, and propose wording changes to that policy if needed.
3. **Curator provider and data egress.** Decide:
   - The provider interface.
   - Ollama as the default.
   - Whether the Anthropic adapter ships in the first version, how operators enable it for each scope, and which model IDs it uses.
   - What leaves the deployment: redacted excerpts only.
   - Cost and rate caps.
   - Prompt framing for untrusted input.
4. **Curator output contract.** Decide:
   - Which memory types the curator may propose.
   - The citation format (`source_uri` pointing to a session excerpt).
   - Deduplication against existing memories, for example by reusing `TEAM_MEMORY_CONFLICT_SIMILARITY`.
   - A cap on proposals per scope per run.
   - Batching for reviewers.
   - The evaluation metrics and the duplicate-rate threshold that stage 03 must meet.
5. **Skill format and distribution.** Decide:
   - What a skill is: an ordered set of approved revision IDs plus metadata.
   - How `SKILL.md` is rendered.
   - Versioning.
   - Which role approves a skill version.
   - How clients install it: the paths for Claude Code and Codex.
   - How auto-update works.
   - Integrity: checksums, and whether the manifest is signed.
   - What happens when a member revision is superseded or archived.
6. **Client CLI and path browser.** Decide:
   - Whether the client CLI is a subcommand group in the existing binary or a separate binary.
   - Authentication reuse (PAT, OAuth device or browser flow).
   - The path grammar for the read-only browser.
   - Which operations are read-only.
   - How wiki links between memories are stored: a relations table, or links parsed from body text.
7. **standards-api contract.** Choose the sync direction:
   - (a) Team Memory pushes approved conventions to standards-api.
   - (b) standards-api pushes its standards into Team Memory as approved, read-only memories.
   - (c) Both, with a clear owner for each field.

   Then define:
   - The field mapping, including how `severity`, `category` and `appliesTo` are represented in Team Memory.
   - How `ruleKey` and versions are generated.
   - Idempotency and drift detection.
   - The credentials. standards-api has one shared write key; decide whether it needs per-client keys or OAuth first.

   Prefer (a) unless the evidence points elsewhere. It keeps approval in one place.
8. **Landscape refresh.** Re-check `docs/landscape.md` for projects that combine capture with governed approval. Update the table only with claims you can source.

Do not write migrations, code or prompts for later stages here. Correct later prompts only where a decision invalidates them, and list each correction in the handoff.

## Deliverables

- New decision records in `docs/decisions/`. Suggested names: `session-capture.md`, `transcript-retention.md`, `curator-provider.md`, `curator-output.md`, `skills.md`, `client-cli.md`, `standards-api-contract.md`.
- Updates to `docs/security/threat-model.md` covering the new assets and trust boundaries: transcripts, curator egress, skill delivery, standards sync.
- `docs/landscape.md` refreshed if anything changed.
- `docs/handoff/experience-loop-01.md`.

## Acceptance criteria

- Every topic above has a record with a chosen option, or an explicit open question the author has been asked.
- The threat model names each new asset, who can read it, and the controls that protect it.
- The curator output contract includes numeric caps and an evaluation threshold that stage 03 can test.
- The standards-api contract has a field-by-field mapping table and a named sync direction.
- No decision allows any automated path to approve a revision.

## Handoff

Record in `docs/handoff/experience-loop-01.md`:

- The chosen options and the record for each.
- Whether the Anthropic adapter is in scope.
- The sync direction, and any work required in standards-api first.
- Changes to later prompts.
- Remaining open questions.
