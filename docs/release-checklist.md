# Release checklist

Evidence as of 2026-10-23. "Done" means a command or document in the repo shows it, or the author reported it (marked). Re-run the commands before publishing.

Team Memory is released as a **personal proof of concept**: no support, no production or sensitive-data use claimed. That framing removes the external security review and the real-project pilot from the release gates.

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
- [x] External security review: not planned; out of scope for a personal proof of concept (stated in README and SECURITY.md).

## Operations
- [x] `helm lint --strict` and template pass (`make helm-lint`); alert rules pass `promtool` tests (`make check-alerts`).
- [x] Restore drill with measured numbers: `docs/operations/restore-drill-report.md`.
- [x] Load test with environment and percentiles: `docs/operations/load-test-report.md`. Write p95 at concurrency 32 is **not reliably under 300 ms** on the test laptop; end-to-end search including CPU-only embedding is far above 500 ms (no target). Re-measure on the real cluster.
- [x] Rollback exercised once: `docs/operations/upgrade.md`, `make rollback-drill` (stage 06 handoff).
- [ ] **CI green on GitHub: the first runs failed on 2026-10-23** (govulncheck on GO-2026-6809 in Go 1.27.2; `gitleaks-action` needs a license for organization repositories). Fixed in the first pull request (Go 1.27.3, gitleaks CLI); confirm a green run on `main`. **Blocks tagging.**

## Client integration
- [x] Early PAT smoke test, both clients (`docs/verification/early-clients.md`).
- [x] OAuth login, both clients (`docs/verification/stage-03-oauth.md`).
- [x] Cross-client A-to-B scenario, both directions: passed, reported by the author on 2026-10-23 (`docs/verification/cross-client.md`; versions and outputs not recorded).
- [ ] Record the client versions used in the 2026-10-23 cross-client run (2.1.292 is the last recorded Claude Code version).
- [ ] Following README and client docs literally on a clean machine: partly done. A fresh `git clone` + `go build ./...` + `go vet ./...` (2026-10-22), and the README from-source quickstart against a fresh database through MCP and portal sign-in page (2026-10-23).

## Release hygiene
- [x] LICENSE (Apache 2.0), NOTICE, README, CONTRIBUTING, CODE_OF_CONDUCT, SECURITY, CHANGELOG, issue/PR templates, versioning policy, CI extension.
- [x] SECURITY.md and CODE_OF_CONDUCT contact set (GitHub private reporting, or the author's public email).
- [x] Module path matches the intended repository URL.
- [x] Planning documents ship publicly; local paths and personal-cluster references removed (`docs/verification/repo-audit.md`).
- [x] Helm chart: strict lint, `values.schema.json`, icon and metadata, OCI install docs, tag-triggered release workflow (`.github/workflows/release.yml`; inert until a `v*.*.*` tag is pushed). Never installed from the OCI registry: no release exists yet.
- [ ] Landscape refresh (`docs/landscape.md` is from stage 01; not refreshed): needs cited web research.
- [x] Private pilot on a real project: not required for a personal proof of concept and not claimed (`docs/verification/private-pilot.md`).

## Dependency licenses
29 Apache-2.0, 21 BSD-3, 2 BSD-2, 19 MIT, 1 Unlicense; MPL-2.0 (`filepath-securejoin`, file-level) and MIT-0 (`segmentio/asm`) confirmed from module LICENSE files. No GPL/AGPL.

## Recommendation: GO once CI is green
Cross-client verification passed (author-reported), and the project is positioned as a personal proof of concept. The remaining blocker is CI on GitHub: fix it, confirm a green run on `main`, then tag. The unchecked landscape refresh, client-version record and clean-machine walkthrough are nice to have, not blockers. Pushing a `v*.*.*` tag publishes the image and chart to GHCR immediately.
