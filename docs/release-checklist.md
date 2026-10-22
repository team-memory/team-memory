# Release checklist

Evidence only, as of 2026-10-22 on `main`. "Done" means a command or document in the repo shows it. Re-run the commands before publishing.

## Core service
- [x] Build, vet and tests pass against real PostgreSQL: `make test` (race detector) passed on 2026-10-22.
- [x] Lint clean: `make lint` reports 0 issues.
- [x] Retrieval measured: `docs/eval/retrieval-report.md`.
- [x] `internal/memory` does not import `internal/mcp` (test) and no `git` exec (`TestNoProcessExecution`).

## Security
- [x] RLS enabled and forced on tenant tables; adversarial suite passes (`integration/security/`).
- [x] Secret scan clean in tree and history: `make secret-scan`.
- [x] `govulncheck`: 0 reachable vulnerabilities; one unreachable, unfixable advisory GO-2026-5932 (see `docs/verification/repo-audit.md`).
- [x] Threat model with residual risks: `docs/security/threat-model.md`.
- [ ] External security review: not done (recommended before sensitive use).

## Operations
- [x] `helm lint --strict` and template pass (`make helm-lint`); alert rules pass `promtool` tests (`make check-alerts`).
- [x] Restore drill with measured numbers: `docs/operations/restore-drill-report.md`.
- [x] Load test with environment and percentiles: `docs/operations/load-test-report.md`. Write p95 at concurrency 32 is **not reliably under 300 ms** on the test laptop; end-to-end search including CPU-only embedding is far above 500 ms (no target). Re-measure on the real cluster.
- [x] Rollback exercised once: `docs/operations/upgrade.md`, `make rollback-drill` (stage 06 handoff).
- [ ] CI run on GitHub: the workflow was extended (lint, govulncheck) but has never run on GitHub; local equivalents pass. The container image builds (`docker build`, 2026-10-22).

## Client integration
- [x] Early PAT smoke test, both clients (`docs/verification/early-clients.md`).
- [x] OAuth login, both clients (`docs/verification/stage-03-oauth.md`).
- [ ] **Cross-client A-to-B scenario, both directions, with portal approval, hierarchy, promotion and mirror checks: not run** (`docs/verification/cross-client.md`). **Blocks release.**
- [ ] Re-test with current Claude Code (2.1.304 installed; 2.1.292 verified).
- [ ] Following README and client docs literally on a clean machine: not done. Only a fresh `git clone` + `go build ./...` + `go vet ./...` was verified (2026-10-22).

## Release hygiene
- [x] LICENSE (Apache 2.0), NOTICE, README, CONTRIBUTING, CODE_OF_CONDUCT, SECURITY, CHANGELOG, issue/PR templates, versioning policy, CI extension.
- [x] SECURITY.md and CODE_OF_CONDUCT contact set (GitHub private reporting, or the author's public email).
- [x] Module path matches the intended repository URL.
- [x] Planning documents ship publicly; local paths and personal-cluster references removed (`docs/verification/repo-audit.md`).
- [x] Helm chart: strict lint, `values.schema.json`, icon and metadata, OCI install docs, tag-triggered release workflow (`.github/workflows/release.yml`; inert until a `v*.*.*` tag is pushed). Never installed from the OCI registry: no release exists yet.
- [ ] Landscape refresh (`docs/landscape.md` is from stage 01; not refreshed): needs cited web research.
- [ ] Private pilot on a real project: not started (`docs/verification/private-pilot.md`).

## Dependency licenses
29 Apache-2.0, 21 BSD-3, 2 BSD-2, 19 MIT, 1 Unlicense; MPL-2.0 (`filepath-securejoin`, file-level) and MIT-0 (`segmentio/asm`) confirmed from module LICENSE files. No GPL/AGPL.

## Recommendation: NO-GO
Required cross-client verification is not done (the author will test Codex later), and the author's own-project pilot has not started. Everything else buildable without a live cluster is done and green.
