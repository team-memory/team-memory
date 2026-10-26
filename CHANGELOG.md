# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/) once a first version is tagged (see `CONTRIBUTING.md`).

## [Unreleased]

### Added
- Screenshot walkthrough of a synthetic MCP and portal test drive, illustrated README and portal guide, and `make docs-screenshots` to regenerate captures against a disposable test database.
- Per-revision MCP client attribution, OAuth application and PAT audit references, and a maintainer audit page with actor, client, action, outcome, target, and date filters.
- Branded public homepage with agent setup instructions for Codex and Claude Code, deployment-aware authentication guidance, and portal links.

### Changed
- Refreshed portal and OAuth consent styling with responsive layouts and shared branding.

### Fixed
- Client attribution now falls back to the HTTP `User-Agent` (labelled "HTTP user agent") when an MCP client sends no `clientInfo`, which stateless serving otherwise loses for 2025-11-25 clients.
- Helm ingress now routes the public homepage and welcome page assets, using an exact root route to avoid exposing operational endpoints.
- Search now preserves the Include overridden checkbox after submitting filters.
- Release workflow: the image is now also tagged with the version without the `v` (for example `0.1.1`), which is the tag the Helm chart deploys by default.
- Release workflow: the image is built with `VERSION`, so `team-memory version` reports the release instead of `0.0.0-dev`.
- Release workflow: the image is built for `linux/amd64` and `linux/arm64` (0.1.0 was amd64 only).

## [0.1.0] - 2026-10-23

First tagged release of a personal proof of concept. Built in stages; see `docs/handoff/` for what each stage delivered.

### Known issues
- The chart's default image tag, `0.1.0`, was not published; install with `--set image.tag=v0.1.0`. The image reports version `0.0.0-dev` and is amd64 only. Fixed in the next release.

### Security
- Built with Go 1.27.3, which fixes GO-2026-6809 in `crypto/x509`.

### Changed
- Positioned as a personal proof of concept: no external security review or real-project pilot is planned (README, SECURITY.md, release checklist).
- README reorganized; setup docs corrected against the code.

### Added
- Per-revision MCP client attribution, OAuth application and PAT audit references, and a maintainer audit page with actor, client, action, outcome, target, and date filters.
- Isolated local Docker app/database stack and reproducible setup, Codex connection, testing and shutdown instructions.
- Helm chart schema, icon and metadata; tag-triggered release workflow for the image and chart.
- CONTRIBUTING, CODE_OF_CONDUCT, SECURITY, NOTICE, issue and PR templates, client setup docs, release checklist (stage 07).
- Memory engine with immutable revisions, hierarchical scopes and downward-only inheritance (stage 01).
- Hybrid full-text and vector retrieval with durable embedding jobs (stage 02).
- OAuth 2.1 authorization-server facade with upstream OIDC, plus personal access tokens (stage 03).
- Row-level security, revision-bound review and promotion, audit log (stage 04).
- Web portal for browsing, searching, reviewing and promoting memories (stage 05).
- One-way git mirror, Helm chart, pgBackRest backups, metrics and tracing, runbooks (stage 06).
