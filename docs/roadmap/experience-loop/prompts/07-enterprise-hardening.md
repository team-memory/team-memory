# 07: Enterprise hardening

## Objective

Make the experience loop safe to run for an organization. Before release, bring its security, operations and data handling up to the standard of the existing service. Transcripts and curator egress add the most sensitive data and the widest trust boundary Team Memory has had, so they need the most attention.

## Context

Team Memory is a self-hosted Go service. It ships as one distroless image and a Helm chart with NetworkPolicies, Prometheus metrics, OpenTelemetry traces, pgBackRest backups, load tests and runbooks. Stages 02–06 added:

- Session capture.
- A curator with pluggable LLM providers.
- Skills with CLI distribution.
- CLI and wiki views.
- standards-api sync.

## Inputs

- `docs/handoff/experience-loop-01.md` through `-06.md`. If any is missing, stop and ask which stages were skipped.
- `docs/security/threat-model.md`, `docs/operations/`, `deploy/`, `loadtest/`, `integration/security/`, `docs/release-checklist.md`

## Work

1. **Threat model review.** Walk each new asset and trust boundary in the threat model against the code as built. Add any missing controls or tests. Record residual risks plainly.
2. **Adversarial sweep.** Extend `integration/security/` with tests across features:
   - A user who loses a role mid-flow (in capture, curation, skill sync and standards sync).
   - Scope moves and deletions.
   - Token revocation during a long-running sync.
   - A malicious skill manifest.
   - Curator injection combined with links across scopes.
3. **Data handling.**
   - Document what is stored, where, for how long, and how to delete it, in `docs/security/data-handling.md`.
   - Add an operator command that exports or deletes all data attributed to one principal, if stage 01 or the threat model calls for it.
   - Verify that backups respect retention, or document how they do not.
4. **Audit export.** Make sure every new action is in the audit log. If the existing log has no export to a SIEM, add a documented way to stream or export it, for example JSON lines over the existing pipeline. Do not add a new external dependency without recording the decision.
5. **Deployment.**
   - Add Helm values, NetworkPolicies and probes for the curator and sync workers. Egress to Ollama, Anthropic and standards-api is allowed only when configured.
   - Extend `make helm-lint` coverage.
6. **Observability.** Add metrics and example alerts for:
   - Ingest volume and rejections.
   - Redaction hits.
   - Curator runs, cost, failures and queue depth.
   - Pending review age.
   - Skill sync failures.
   - Standards sync dead letters.
7. **Load.** Extend `loadtest/` with session ingest and curator throughput. Record the measured limits and the sizing guidance.
8. **Runbooks.** Write runbooks for:
   - Disabling capture in an emergency.
   - Purging a leaked secret from sessions and proposals.
   - Pausing the curator.
   - Rolling back a skill version.
   - Recovering standards sync.

## Deliverables

- Tests, Helm changes, alerts, load-test scripts and results.
- `docs/security/data-handling.md`, the updated threat model and the runbooks in `docs/operations/`.
- Release checklist updates and a `CHANGELOG.md` entry.
- `docs/handoff/experience-loop-07.md`.

## Acceptance criteria

- `make test`, `make test-security`, `make lint`, `make helm-lint` and `make test-e2e` pass.
- Every new asset in the threat model has a named control and at least one test, or is listed as a residual risk.
- A rendered Helm chart with all new features disabled has the same network policy as 0.1.x. Enabling each feature opens only its documented egress.
- Load-test results and sizing guidance are recorded with the hardware they ran on.

## Handoff

Record in `docs/handoff/experience-loop-07.md`:

- Residual risks.
- Measured limits.
- Which release checklist items are done and which are still open.
- Anything that blocks release.
