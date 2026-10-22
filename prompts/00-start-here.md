# Start Here: Team Memory build guide

Each prompt in this folder has a defined input bundle and is written for a fresh assistant session. Run them in order unless noted. Producing these prompts does not authorize deploying, spending money, contacting people, or publishing anything; each prompt says what it may do.

## Order and dependencies

| # | File | Depends on | Notes |
| --- | --- | --- | --- |
| 1 | `01-foundation.md` | none | Checks alternatives, sets up the service code in this repository and early client smoke test. |
| 2 | `02-intelligent-retrieval.md` | 01 | Retrieval and evaluation. |
| 3 | `03-identity-provider-decision.md` | 01 (can run in parallel with 02) | Discovery. Decides and proves the auth architecture (solo tokens plus bring-your-own-SSO). Must finish before 04. |
| 4 | `04-team-security.md` | 01, 02, 03 | Security and review services. |
| 5 | `05-portal.md` | 04 | Web portal for browsing, searching, and reviewing memories across the scope hierarchy. |
| 6 | `06-operational-hardening.md` | 04, 05 | Operations, mirror, recovery and load testing. |
| 7 | `07-client-verification-and-release.md` | 06 | Cross-client milestone, OSS release prep. Publishing stays a manual decision. |

## Where things live

- One repository holds everything: Planning artifacts (`proposal.md`, `review*.md`, `prompts/`) and the service code (`cmd/`, `internal/`, `migrations/`, ...) live side by side at its root.
- Decision records: `docs/decisions/`
- Handoff notes: each stage writes `docs/handoff/NN-<stage>.md`. Attach the previous stage's handoff to the next session.

## What to attach to each session

1. The prompt file for that stage.
2. `proposal.md` (context and scope) and `review-decisions.md` (accepted security and lifecycle contract).
3. The previous stage's handoff note (and for 04, also `docs/decisions/identity-provider.md`).
4. This guide (`prompts/00-start-here.md`), including its global rules, and access to the repository.

Use every prerequisite listed in the stage prompt, not only the immediately preceding handoff. All seven stages are required for the agreed v1; extra embedding-model evaluation is conditional on availability. Real deployment and publishing remain separately authorized actions. Stage 03 may run alongside 02 after 01, using isolated changes and reconciling handoffs before 04.

If a session cannot see an attachment it needs, the prompt tells the assistant to ask for it rather than invent its contents.

## Fixed decisions (from the author's brief)

Go modular monolith; MCP over Streamable HTTP; PostgreSQL 17+ with pgvector; pgx and sqlc; pluggable embeddings with local Ollama first; OIDC/OAuth 2.1 with individual identities; RLS plus application checks; Hierarchical scope tenancy (default organization → team → project, admin-defined extra levels, memories flow down; the only way up is an explicit human-approved promotion one level at a time; conflicts resolve to the lowest scope); Kubernetes via Helm; OpenTelemetry and Prometheus; Apache 2.0. PostgreSQL is the source of truth; a one-way git mirror (stage 06) exports each exportable event to a private scope repo for history and supplementary recovery. Git is never imported during normal operation; explicitly invoked disaster recovery is the documented exception. The mirror uses the go-git library (https://github.com/go-git/go-git), not the `git` binary. The Go MCP SDK is the suggested starting point; stage 01 confirms it against current MCP spec support.

V1 owner bootstrap is CLI-only through `team-memory init`. Later SSO linking requires explicit authentication to both identities; browser setup tokens and preconfigured SSO-owner bootstrap are deferred.

## Unresolved at the time of writing

- Auth architecture: working assumption is a Team Memory authorization-server facade with upstream OIDC and personal access tokens; stage 03 confirms it with evidence. IdP credentials are supplied as Kubernetes Secrets before first sign-in.
- Which Go MCP SDK to use (stage 01 confirms).
- Embedding model beyond the `nomic-embed-text` candidate (stage 02 evaluates).
- Competitive landscape check (initial review in stage 01, refreshed in stage 07).

## Global rules for every stage

- Do not hardcode private hostnames, credentials, or personal details; use placeholders like `memory.example.com`.
- Never commit secrets. Use environment variables and Kubernetes Secrets.
- Verify claims about the MCP specification, OAuth requirements, and client behavior against current official sources and the installed client versions; do not rely on memory.
- Keep the memory engine independent of the MCP adapter (`internal/memory` must not import `internal/mcp`).
- Report test failures and skipped checks honestly in the handoff note.

## Pilot and completion gates

The first private pilot is one project used by the author across multiple machines on an existing Kubernetes cluster. Use distinct test identities to verify role boundaries; do not call this evidence of multi-person adoption. Git mirrors are operator-only in v1; users browse history through the service.

A failed or skipped required acceptance check means the stage is incomplete, even when the findings are documented. Carry a blocker into the handoff rather than reporting success. Stage 03 needs a working selected authentication path before 04; stage 07 prepares a no-go report if required cross-client or security checks fail. Performance misses remain measured gaps, not fabricated passes. Public publishing is outside these prompts.

The author has no self-hosted SSO and no selected pilot project yet. Use disposable local Keycloak for development; choose a pilot project before stage 07, without blocking foundation work. The release priority is GitHub launch as soon as the required stability checks pass.

The earlier week labels were estimates, not deadlines. Re-estimate after stages 01 and 03; retain the agreed scope unless the author changes it.
