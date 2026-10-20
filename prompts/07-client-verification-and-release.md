# Stage 07: Client Verification and Open-Source Release Prep

## Objective
Prove the defining milestone — Developer A records a decision in Claude Code, Developer B retrieves it in Codex, and both see author, rationale, and approval status — and prepare the repository for an open-source release. Publishing itself is a manual decision for the user.

## Context
Team Memory is an open-source (Apache 2.0) Go MCP service for shared, versioned, permission-aware agent knowledge on PostgreSQL + pgvector (see `proposal.md`). Stages 01–06 built the service, retrieval, identity, security, the web portal, and operations. The launch audience is prospective users and contributors; the repo must be understandable and safe to try without access to the author's private environment.

## Inputs
- `prompts/00-start-here.md` (attach; follow its global rules and completion gates).
- `review-decisions.md` (attach with the proposal; required accepted security/lifecycle decisions).
- `proposal.md` and `docs/handoff/01-foundation.md`, `docs/handoff/02-retrieval.md`, `docs/handoff/03-identity.md`, `docs/handoff/04-security.md`, `docs/handoff/05-portal.md`, `docs/handoff/06-operations.md`, plus `docs/decisions/identity-provider.md` (attach). Ask for anything missing.
- A running deployment or local stack (the user provides its URL and authorized test identities for writer, reader, source/destination maintainer, and unauthorized-user scenarios; one identity may hold both maintainer roles, but writer/reader/unauthorized checks must remain distinct; never invent credentials).
- Installed Claude Code and Codex CLIs, versions noted.

## Work
1. **Cross-client scenario test:** with two distinct test users, perform and record the scenario end to end:
   a. User A (writer) in Claude Code creates a `decision` memory with rationale and a source link.
   b. Maintainer explicitly approves the exact revision in the portal; verify `memory_approve` only requests review and cannot finalize it.
   c. User B (reader) in Codex searches "why did we choose X?" and sees content, author, rationale, source, revision, and `approved`.
   d. User A updates it (new revision) and the trust/approval behavior matches `docs/decisions/trust-rules.md`.
   e. A maintainer supersedes it; default search no longer returns it, `include_superseded` does.
   f. A user without access to the project gets nothing.
   f2. Hierarchy: an organization-level convention created by an org maintainer appears, labeled inherited, in a project search from both clients and in the portal for a project reader; a project memory does not appear in the team or org search or portal views for any user lacking access to that project. Then a project maintainer approves a project memory in the portal and submits it for promotion, then a destination maintainer accepts the snapshot in the portal: a team-level user can read it, project searches from both clients show it as inherited with no duplicate, and the mirror has the matching commits. Finally a project memory proposed as overriding an inherited team memory initially leaves both visible; after approval of the exact revision/link, it wins in project searches from both clients (the team memory is flagged `overridden_by` when requested) while team-level searches are unchanged.
   g. Confirm each exportable mutation above appears as an event-identified commit in the correct scope mirror repo with the right author and trust level. Reads and permission denials do not create mirror commits.
   Repeat the reverse direction (create in Codex, retrieve in Claude Code). Capture exact commands, versions, and outcomes. Where a client behaves differently from the spec, document the workaround or file it as a known limitation; do not claim success without observing it.
2. **Client setup docs:** `docs/clients/claude-code.md` (e.g. `claude mcp add --transport http --scope user team-memory https://memory.example.com/mcp`) and `docs/clients/codex.md` (`~/.codex/config.toml` entry), with OAuth steps as actually verified, and troubleshooting.
3. **Agent policy files:** finalize `docs/agent-policy/AGENTS.md` and `CLAUDE.md` snippets (search before architectural changes, prefer approved sources, verify against the repo, propose lessons with links, no secrets, repo and git docs take precedence over memories).
4. **Release hygiene:** `README.md` (what it is, the scope hierarchy and downward-only inheritance, quickstart, architecture overview, honest status and limitations), `LICENSE` (Apache 2.0, confirm headers/NOTICE conventions), `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md` (private reporting channel — ask the user which contact to use), issue/PR templates, `CHANGELOG.md`, versioning policy, extend the stage 01 CI workflow (build, test with Postgres service, lint, `govulncheck`, container build, Helm lint).
5. **Repo audit:** scan the whole repository and git history for secrets, private hostnames/IPs, personal data, and internal-only references; list findings and fix or confirm each. Check dependency licenses are compatible with Apache 2.0.
6. **Landscape check:** refresh stage 01 research on existing open-source MCP memory servers and shared agent-memory projects; update `docs/landscape.md` comparing honestly (what overlaps, what differs). Do not invent comparisons; cite sources and dates.
7. **Launch checklist:** the author wants to launch on GitHub as soon as stable, rather than wait for a fixed date. Base the recommendation on passing required checks and recorded operational gaps, not elapsed time or unverified adoption. Write `docs/release-checklist.md` mapping to the production-readiness items: core service, security, operations, client integration. Mark each item done/not done based on evidence in the repo, not intention.

8. **Private pilot:** no project is selected yet. Before running the pilot, ask the author to select one; Team Memory itself is a possible dogfooding option. Do not invent a project or treat synthetic fixtures as real use. Use the author’s selected project across at least two machines against the authorized Kubernetes deployment. Carry a real decision from one machine/client to another, inspect its source and review status, update it, and verify the subsequent session sees the new state. Record setup friction, useful and missed retrievals, stale guidance, and review effort in `docs/verification/private-pilot.md`. Distinguish actual observations from suggestions; do not invent time savings or claim this single-person pilot proves team adoption. If access or observation time is unavailable, leave the pilot incomplete and state what remains. Include a proposed later multi-person pilot, without contacting participants.

## Excluded
Creating a public GitHub repository, pushing, tagging, posting announcements, or contacting anyone. Prepare the materials and stop; the user decides when to publish.

## Deliverables
Cross-client verification report (`docs/verification/cross-client.md`), private pilot report (`docs/verification/private-pilot.md`), client docs, agent policy files, release hygiene files, CI config, repo audit report, landscape doc, release checklist, `docs/handoff/07-release.md`.

## Acceptance criteria
- The cross-client report shows passing observed outputs for each required scenario in both directions. Failed or unavailable checks are documented as incomplete and result in a no-go release assessment, not a completed verification gate.
- A new person can follow the README and client docs on a clean machine to run the stack and connect one client (test by following them literally, in a fresh clone/directory).
- CI config runs locally or in a dry run where possible; failures are reported.
- Repo audit has no unresolved secret or private-host findings.
- The release checklist reflects evidence only; unchecked items are listed with reasons.

- The private pilot records actual cross-machine observations on the same project, clearly separated from synthetic identity tests and untested team-adoption hypotheses.

## Handoff
`docs/handoff/07-release.md`: go/no-go summary for publishing, open issues, recommended external security review, and the remaining manual steps for the user (create repo, set security contact, tag release, announce).
