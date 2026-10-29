# 04: Skills

## Objective

Let maintainers package approved memories as versioned `SKILL.md` skills that agents install and keep current. Stash shows that skills which update themselves are how lessons actually reach the next run. Team Memory's version must deliver only human-approved revisions, and only to people who can read the skill's scope.

## Context

Team Memory is a self-hosted Go service (PostgreSQL 17, forced RLS, organization → team → project scopes inherited downward, approval bound to an exact revision and possible only in the portal, an append-only audit log). This stage does not depend on capture or the curator. A skill can contain any approved memory, whether a person or the curator wrote it.

## Inputs

- `docs/handoff/experience-loop-01.md` and the skills decision record. If either is missing, stop and ask for them.
- `internal/memory/`, `internal/portal/`, `internal/mcp/` (resource views), `docs/decisions/trust-rules.md`, `docs/decisions/scope-tree.md`, `docs/agent-policy/`
- The client skill locations for Claude Code and Codex recorded in stage 01. Check them against current client documentation.

## Work

1. **Data model.** Add forward-only migrations for skills and their versions, following stage 01's model, with forced RLS.
   - A skill version references exact approved revision IDs, plus ordering, title, description and optional supporting text.
   - A skill version is immutable once published.
2. **Composition and approval.** In the portal:
   - Let a maintainer create a skill in a scope and add approved memories that are visible from that scope, including inherited ones.
   - Show a preview of the rendered `SKILL.md`.
   - Let them publish a version. Publishing is a browser-only, revision-bound action like memory approval, with an audit event.
   - Draft skills are never served to agents.
3. **Rendering.** Render `SKILL.md` deterministically from a skill version. Include frontmatter (name, description, version), each memory's text, origin scope and revision, and a note that the content is reference data from Team Memory.
   - Produce a manifest with checksums, and sign it if stage 01 decided to.
4. **Lifecycle.** When a member revision is superseded or archived:
   - Mark the skill version stale in the portal and prompt for a new version.
   - Never silently swap in unapproved text.
5. **Distribution.**
   - Serve published skills through an MCP resource view and an authenticated HTTP endpoint.
   - Add `team-memory skills list|install|sync` (in the client CLI shape stage 01 chose). It writes skills into the right Claude Code and Codex locations, checks checksums and records installed versions locally.
   - Add a session-start hook example that runs `sync`, fails open and never blocks the agent.
6. **Dogfood.** Turn this repository's `docs/agent-policy/` guidance into a skill in the test-drive fixtures, as a working example.
7. **Tests.**
   - Rendering golden tests.
   - Integration tests for publish, sync and stale detection.
   - Adversarial tests: a skill that includes a revision from a scope the publisher cannot read; an unapproved revision forced in through the API; a tampered manifest rejected by `sync`; a reader in a sibling scope.

Excluded: public publishing, forking and a discovery catalog.

## Deliverables

- Migrations, the domain package, portal pages, the renderer, MCP and HTTP distribution, CLI commands, hook examples and tests.
- `docs/skills.md` (a user guide) and a client setup section in `docs/clients/`.
- A `CHANGELOG.md` entry.
- `docs/handoff/experience-loop-04.md`.

## Acceptance criteria

- `make test`, `make test-security`, `make lint` and `make test-e2e` (for the portal flow) pass.
- A published skill installs into both Claude Code and Codex, and `sync` picks up a new version.
- A skill can never contain or serve an unapproved revision, and the tests prove it.
- A user without read access to the skill's scope gets nothing from any distribution path.

## Handoff

Record in `docs/handoff/experience-loop-04.md`:

- The CLI command shape and how it authenticates, which stage 05 reuses.
- The install paths for each client.
- Manifest format.
- The stale-version workflow.
- Open issues.
