# 05: CLI and wiki views

## Objective

Give people and agents ways to reach Team Memory beyond MCP: a client CLI with a read-only, path-style browser, and portal wiki pages where memories link to each other and to the sessions behind them. Stash's virtual filesystem and linked wiki make knowledge easy to explore. This stage adds the same convenience on top of Team Memory's permissions.

## Context

Team Memory is a self-hosted Go service (PostgreSQL 17, forced RLS, scopes inherited downward, approval only in the portal). Stage 04 added a client CLI command group for skills. Stage 03's curator produces proposals whose `source_uri` cites session excerpts. If stages 02 and 03 are not done yet, build everything except session citations and record that gap.

## Inputs

- `docs/handoff/experience-loop-01.md` (client CLI decision) and `docs/handoff/experience-loop-04.md` (CLI shape and authentication). Use `docs/handoff/experience-loop-03.md` for the citation format if it exists. If 01 or 04 is missing, stop and ask for it.
- `cmd/team-memory/`, `internal/search/`, `internal/memory/`, `internal/portal/`

## Work

1. **Client CLI.** Extend the CLI chosen in stage 01 with:
   - `search`, `get`, `history` and `recent`, matching the MCP tool semantics and output, including `origin_scope`, `inherited` and trust state.
   - `propose`, which creates an unverified memory. It is optional, and only if stage 01 allows writes from the CLI.
   - Output as text or JSON for scripting.
   - Reuse PAT and OAuth authentication. Never print tokens.
2. **Path browser.** Implement stage 01's path grammar, for example `team-memory browse ls /acme/platform/conventions` and `cat /acme/platform/conventions/<slug>`.
   - It is read-only, filtered by the caller's scope chain and backed by the same queries as search.
   - Add a `grep`-style full-text filter if stage 01 included one.
3. **Memory links.** Add links between memories as stage 01 decided: a relations table with forced RLS, or links parsed from text.
   - A link is visible only when the reader can see both ends. A link must never reveal that a target the reader cannot see exists.
4. **Portal wiki views.** Add the following to the portal:
   - A page for each scope that groups approved memories by type.
   - Memory pages with outgoing links and backlinks.
   - Rendered session citations from curator proposals, when the reader may view the session.
   - Clear trust badges (approved, unverified).
   - Use the existing portal templates and styles.
5. **Tests.**
   - CLI golden tests.
   - Integration tests for browse filtering.
   - Adversarial tests: links and backlinks across scope boundaries; path traversal and odd paths in the browser; citations to sessions the reader cannot see.
   - Playwright coverage for the wiki pages.

Excluded: write access through the path browser, and a FUSE mount or real filesystem.

## Deliverables

- CLI commands, the browser, migrations if needed, portal templates and tests.
- `docs/cli.md`, plus updates to `docs/portal.md`. Regenerate the portal screenshots with `make docs-screenshots` if pages changed.
- A `CHANGELOG.md` entry.
- `docs/handoff/experience-loop-05.md`.

## Acceptance criteria

- `make test`, `make test-security`, `make lint` and `make test-e2e` pass.
- For the same user, the CLI and the MCP tools return the same result set for the same query.
- No link, backlink, citation or browse listing exposes anything from a scope the reader cannot read.

## Handoff

Record in `docs/handoff/experience-loop-05.md`:

- The CLI reference.
- The path grammar.
- The link storage model.
- Any citation gaps.
- Open issues.
