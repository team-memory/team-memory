# Idea-to-build review

Reviewed 2026-10-20 against the idea-to-build workflow. This is a planning review; no service implementation, deployment, client compatibility test, or market research was performed.

## Assessment

The idea, intended audience, and core use case are clear enough to start foundation work. The agreed scope remains a substantial seven-stage v1, with authentication architecture requiring an evidence-backed decision in stage 03. The four-week estimate is unvalidated; stage labels no longer imply a committed schedule. The author wants GitHub launch as soon as stability is demonstrated by the required checks.

No pilot project is selected yet. The first pilot will be the author using one project across multiple machines on an existing Kubernetes cluster. That can demonstrate continuity across clients and machines. It cannot establish multi-person usability or adoption; later team testing remains necessary before making those claims.

## Changes made

- Preserved CLI bootstrap, browser-only review, approved overrides, two-scope promotion consent, cited trust, immutable content revisions, and event-based mirror exports.
- Recorded operator-only git access throughout the proposal and affected stages. End users use service-authorized history; scope-to-git permission synchronization is not part of v1.
- Moved an initial alternatives review and basic PAT cross-client smoke test into stage 01, retaining the full release verification in stage 07.
- Added foundational CI in stage 01; stage 07 extends it.
- Changed auth discovery and release verification so documented failures do not count as passing required completion gates.
- Added the agreed SSO linking flow and its browser tests to stage 05; removed the alternative first-admin-login bootstrap wording from stage 06.
- Corrected local-bare-repository tests so they cannot be mistaken for evidence of SSH/HTTPS authentication support.
- Added a real cross-machine private pilot report, distinct from synthetic authorization scenarios.
- Made shared prompt inputs explicit and aligned missing deliverables and handoff paths.

## Scope coverage and dependencies

| Capability | Stage and evidence |
| --- | --- |
| Memory lifecycle, scopes, tokens, MCP | 01: real PostgreSQL tests, protocol tests, early two-client smoke test |
| Hybrid retrieval and durable embedding work | 02 after 01: retrieval evaluation and index measurements |
| Authentication architecture | 03 after 01, independent of 02: working selected login path and decision record |
| RLS, administration, approval/promotion services | 04 after 01–03: adversarial tests and threat model |
| Browser history, review and SSO linking | 05 after 04: browser and permission tests |
| Operator-only mirror, deployment, backups and recovery | 06 after 04–05: export tests, restore drill, load report and rollback proof |
| Final client verification and release preparation | 07 after 06: both client directions, cross-machine pilot report and evidence-based checklist |

## Remaining execution decisions

- Stage 01 selects the maintained MCP SDK and documents the scope-tree representation and migration tooling.
- Stage 02 evaluates embedding quality/dimensions and the index strategy; no measured quality or performance claims exist yet.
- The author has no self-hosted SSO; stage 03 uses disposable local Keycloak, without selecting a production provider. Stage 03 selects and proves the OAuth/SSO architecture and library. CLI owner bootstrap is already decided and must not be reopened.
- Stage 06 needs actual deployment parameters and authorized access before private-cluster rollout; these are execution inputs, not prerequisites to writing foundation code.
- Stage 07 needs real pilot observations and a security-reporting contact before public release preparation is complete. Publishing remains outside the build prompts.

The proposal and prompt guide preserve the full agreed scope. No additional product decision is required to begin stage 01. This review is not a claim that the system is built, secure, production-ready, or proven useful to teams.
