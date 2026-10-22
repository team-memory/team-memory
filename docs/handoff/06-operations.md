# Handoff: stage 06 (operational hardening)

Date: 2026-10-22. Based on the stage 05 commit `dca9066`. **Not pushed.** CI has still never run; this stage adds chart, alert, image and secret-scan jobs to it.

## Summary

Team Memory is now deployable and operable:

- **Image**: one distroless, non-root, 43 MB image for every component, base images pinned by digest.
- **Helm chart**: `deploy/helm/team-memory`. API, embedding worker, git mirror worker, migration hook, verify CronJob, per-component default-deny NetworkPolicies, PDB, ingress, ServiceMonitor and PrometheusRule.
- **Git mirror** (`internal/mirror`, go-git only): one commit per exportable event, idempotent and serialized per repository, `mirror verify`, and a disaster-recovery rebuild.
- **Observability**: Prometheus metrics, OpenTelemetry traces, and request ids that equal the trace id in logs and audit; 17 example alerts.
- **Backups**: pgBackRest with WAL archiving and a measured restore drill.
- **Load tests**: a Go load tool and report.
- **Rollout and rollback**: drilled with a request probe.
- **Docs**: an authentication deployment guide followed literally in solo and SSO mode, and runbooks.

Everything was exercised on a **disposable k3d cluster** created for this stage (`deploy/dev-k3d/`). No real cluster was touched; no real git remote was used; nothing was pushed or published.

## Acceptance criteria

| Criterion | Evidence | Result |
| --- | --- | --- |
| `helm lint` and `helm template` succeed; deploy reaches ready and passes a smoke test through the ingress | `make helm-lint` (strict, solo and all-features values). k3d: all pods ready under enforced NetworkPolicies; MCP write/search and portal sign-in through Traefik with TLS; `helm test` passes | Pass |
| go-git is the only git implementation; checked by a test; decision record with version, license, limits | `TestNoProcessExecution` (no `os/exec`/`execabs` import anywhere in the service), `TestFileTransportIsInProcess`, CI step; `docs/decisions/git-library.md` (v5.19.3, license table, 8 limits) | Pass |
| Create, update, approve, supersede each produce one correct commit; forced push failure retries without duplicates; verify clean, detects drift | `TestMirrorLifecycleCommits`, `TestMirrorPromotionAndOverrides`, `TestMirrorRetriesWithoutDuplicates` (failure before push, and a push whose acknowledgement is lost), `TestMirrorVerifyDetectsDrift`, `TestMirrorSecretGateHoldsLaterEvents`; on k3d, create/approve exported over SSH to Gitea and the verify Job reported 0 drift | Pass |
| Authenticated SSH and HTTPS against disposable git servers, non-fast-forward | `TestMirrorHTTPSTokenRemote`, `TestMirrorSSHDeployKeyRemote` against Gitea 1.25.3 (`make dev-git-up test-mirror-remote`) | Pass |
| Restore drill succeeded with measured duration and recovery point | `deploy/backup/drill.sh`, [restore-drill-report.md](../operations/restore-drill-report.md) | Pass: checksums of all 20 Team Memory tables identical (including data only in WAL), 73 btree indexes pass amcheck, recovery point 24 s, recovery time 12 s to writable (24 s verified), 120 MB |
| Metrics and traces visible for a sample workload; alerts validated | `TestTelemetrySampleWorkload`; k3d: Prometheus scraped all 4 pods and loaded 17 rules (health ok); Jaeger has traces from all 3 services; `make check-alerts` (promtool check + 6 unit tests) | Pass |
| Load-test report with percentiles versus targets, environment, misses | [load-test-report.md](../operations/load-test-report.md) | Pass (report). Targets: search met; writes met at concurrency 1/8 and in the reported 32 run, **not reliably at 32** (550 ms in an earlier run) |
| Rollback exercised and documented | `deploy/dev-k3d/rollback-drill.sh`: 0.5.0 → 0.6.0 → `helm rollback` → 0.6.0 → failing migration; [upgrade.md](../operations/upgrade.md) | Pass |
| No secrets in chart, image or repo (scan) | gitleaks 8.30.1: working tree, full git history, the image's exported filesystem and the binary's strings: no leaks. Allowlist (`.gitleaks.toml`) only for git-ignored throwaway `.local/` folders and the one fake test key | Pass |
| `authentication.md` followed literally: solo and SSO (Keycloak) sign-in; removing the IdP Secret fails readiness with the documented message | k3d transcript, below | Pass |

## What was built

### Code

- `migrations/00007_mirror.sql`: `team_memory_mirror` role and its column grants and policies; a trigger that enqueues mirror state and jobs for exportable events once `mirror enable` is set; `held`/`purged` states. The embedding worker is narrowed to `embed` jobs.
- `internal/mirror`: document format (JSON-valued YAML frontmatter, exact round trip), go-git implementation (bare clone, direct object writes, minimal-pack push, in-process `file://`), exporter, verify, rebuild. Tests: unit, Gitea, integration.
- `internal/telemetry`: metrics, tracing, pgx tracer, HTTP middleware. Wired into `serve`, `worker`, `mirror worker`. The operations listener on `:9090` serves `/metrics`, `/healthz` and `/readyz`.
- `internal/server`: trusted proxies (`TEAM_MEMORY_TRUSTED_PROXIES`, rightmost untrusted `X-Forwarded-For`), draining readiness, request id = trace id, `X-Trace-Id`.
- `internal/jobs`: last-poll time (worker liveness), batch spans.
- CLI:
  - `mirror enable|disable|status|worker|verify|requeue|mark-purged|rebuild`
  - `jobs dead`
  - `healthcheck [--url] [--wait]`
  - `version`
  - `db create-login --role mirror`, which now migrates first
  - graceful shutdown: `TEAM_MEMORY_SHUTDOWN_DELAY` (3 s by default, 5 s in the chart) and `TEAM_MEMORY_SHUTDOWN_TIMEOUT` (20 s)
  - `TEAM_MEMORY_PORTAL_RATE_LIMIT`/`_BURST`
- `loadtest/`: `seed`, `embed-synthetic`, `run`.

### Deployment and operations

- `Dockerfile`, `.dockerignore`, `.gitleaks.toml`.
- `deploy/helm/team-memory/`: chart, README, `ci/` values, `files/prometheus-rules.yaml`, `scripts/tm-admin.sh`.
- `deploy/observability/alerts-test.yaml`, `deploy/backup/` (pgBackRest image, config, drill), `deploy/operations/purge-memory.sql`.
- `deploy/dev-git/` (disposable Gitea), `deploy/dev-k3d/` (disposable cluster, fixtures, observability, rollback drill).
- Docs: `docs/decisions/git-library.md`, `docs/operations/{authentication,deploy,upgrade,backup-restore,restore-drill-report,rotate-secrets,dead-letters,incident,secret-purge,mirror,observability,load-test-report}.md`, README.
- Makefile targets: `image`, `helm-lint`, `check-alerts`, `secret-scan`, `dev-git-up`, `test-mirror-remote`, `k3d-up`, `k3d-down`, `rollback-drill`, `backup-drill`, `loadtest`. CI: chart, alerts, image, gitleaks and no-exec jobs.

## Deployed topology (disposable cluster)

k3d `tm-dev` (k3s v1.36.4, 1 server + 1 agent, Traefik on `https://*.localtest.me` with a throwaway CA, CoreDNS rewriting those names to Traefik in-cluster, kube-router NetworkPolicy):

- `team-memory` namespace, team mode:
  - 2 API replicas, worker, mirror worker, verify CronJob;
  - SSO via Keycloak 26 (namespace `keycloak`);
  - embeddings from Ollama 0.13 (CPU, a container on the k3d network);
  - mirror over SSH to Gitea 1.25 (`deploy/dev-git`);
  - traces to Jaeger and metrics to Prometheus (namespace `monitoring`).
- `tm-db`: PostgreSQL 17 + pgvector 0.8.1. `tm-rollback` and `tm-load`: drill and load releases with their own databases.

Teardown: `make k3d-down`, `docker compose -f deploy/dev-git/compose.yaml down -v`, `docker rm -f tm-dev-ollama`.

## Authentication guide, followed literally

Solo:

1. Database Secret, then `db create-login` ×3 via `tm-admin.sh`, then patch the URLs.
2. Values, `helm upgrade --install`, rollout ready, `/readyz` `ok`.
3. `init` printed the owner token.
4. Through the ingress:
   - an MCP `memory_create` with the PAT succeeded;
   - no token got 401;
   - `/readyz`, `/healthz` and `/metrics` were not routed (404);
   - portal token sign-in succeeded with a `__Secure-` cookie, and search showed the memory.

SSO, same release:

1. Signing-key Secret and IdP Secret, team values with `tokenSignIn: owner`, upgrade, ready, discovery through the ingress.
2. The owner signed in with the token and used **Connect SSO** with a real Keycloak login (closing the stage 05 gap: `prompt=login`, `max_age=0`, `auth_time` work with Keycloak 26).
3. The owner signed out and back in with SSO only.
4. `eve@evil.test` was refused by the domain rule (403).
5. With `tokenSignIn: auto`, token sign-in returned 401.

Removing `team-memory-idp-keycloak`: about 70 s later (kubelet sync) `/readyz` returned exactly `not ready: sso: provider "keycloak" client secret: open /etc/team-memory/idp/keycloak/client-secret: no such file or directory`. Both pods went 0/1, and MCP returned 503. Recreating the Secret made both pods ready again within about a minute, with 0 restarts.

During this, three defects were found and fixed:

- `tm-admin.sh` lost the output of fast commands (once-only passwords) because `kubectl run --attach` attaches too late; it now reads the pod log.
- The IdP Secret volumes are now `optional`, so a missing Secret yields a not-ready pod with a reason instead of `ContainerCreating`.
- The API's tracing service name was aligned with the chart.

## Measured numbers

- **Mirror** (`git-library.md`): one event 36 ms at 1,000 commits and 77 ms at 5,000, after replacing go-git's history-walking push. Before the fix: 0.56 s and **13 s**. Backfill runs at 58–164 events/s; a cold clone of 5,000 commits takes 24 s.
- **Load** (`load-test-report.md`, 2 API replicas at 250m/256 Mi on a laptop, 32,832 memories, 341 scopes, 5 levels):
  - search p95 excluding query embedding: 8 / 154 / 60 ms at concurrency 1/8/32;
  - write p95: 71 / 87 / 261 ms (an earlier run: 66 / 160 / 550 ms);
  - portal pages p95 ≤ 300 ms at 32;
  - search end to end ~2 s because of CPU-only Ollama.
- **Rollout** (`upgrade.md`): 0 failed requests in 282 during two rolling restarts of 0.6.0; 0 during rollback and during the failing-migration upgrade; 3 per upgrade away from 0.5.0 (its pods do not drain).
- **Restore** (`restore-drill-report.md`): pgBackRest 2.59.3; full backup 10 s for 140 MB; after a SIGKILL of the primary, restore from the repository only was writable in **12 s** (24 s verified) with **24 s** of data lost (RPO target 15 min, RTO 2 h). The mirror DR rebuild was also run by hand: approvals not restored, as designed.

## Gaps against targets, and open items

1. **Write p95 at concurrency 32** is at the edge of the 300 ms target on this hardware (261 ms and 550 ms in two runs). Re-measure on the target cluster; the database (no tuning, shared VM) is the likely limit.
2. **Search end to end with CPU-only Ollama** is about 1–2 s; the target excludes query embedding and is met, but users feel it. A GPU or native Apple-silicon Ollama is recommended. CPU-only backfill runs at about 0.6 documents/s, so **load-test vectors were partly synthetic** (word-embedding sums; queries real).
3. **99.9% availability** is a target, not measured. Single PostgreSQL instance; no standby.
4. **A separate portal hostname is not supported**: the portal's cookies, Origin check and SSO callback are bound to `publicURL`. The prompt asked for one or two hostnames; only one works. Supporting two needs a product change (separate cookie origin and callback routing).
5. **Promotion provenance in the mirror**: the stage 06 prompt asks for `promoted_from` (source scope, memory, revision) in the **destination** repository, but the accepted `mirror-security.md` (stage 04) allows the promotion link "only in the source scope's repository". The decision record was followed: the destination file has `origin: "promotion"` without source ids, and the source's supersession commit names the destination. **Author decision needed** if the prompt's version is preferred; it is a small change in `LoadDocument`.
6. **Review notes are not secret-scanned** (found while testing the purge runbook). The purge procedure redacts them; a scanner check on notes is a small follow-up.
7. **Rate limits are per replica** and the portal sign-in limit is per client address; with `trustedProxies` set, that address is the real client.
8. **NetworkPolicy admission lag**: kube-router admitted a new pod's traffic to podSelector-protected peers 1–3 s after start; `helm test` retries. Other CNIs may differ.
9. **Not done**:
   - no deploy to a real cluster (needs explicit confirmation and the values noted in `deploy.md`);
   - no real git host (GitHub/GitLab) or hosted IdP (Google/Entra recipes are written from current official docs but untested);
   - no S3 backup repository or encryption in the drill (POSIX repository);
   - no point-in-time restore to an earlier time (latest only);
   - Playwright browser tests were not re-run (the portal changes in this stage are config-only).
10. **`jobs retry-dead`** requeues all dead jobs of a kind; there is no per-job replay.
11. **go-git v5 limits** are listed in `git-library.md`; the in-process `file://` server has no compare-and-swap (safe only with the advisory lock).

## Tests

- `go test -race -count=1 ./...` against PostgreSQL 17 + pgvector (Docker Desktop), with the dev Gitea available: **152 top-level tests passed, 0 failed, 4 skipped**. Skipped (env-gated, as before): `TestRetrievalEval`, `TestVectorIndexStrategies`, `TestPortalBrowserEndToEnd` (Playwright, not re-run), `TestMirrorThroughput` (run separately with `TEAM_MEMORY_MIRROR_BENCH=1000,5000`, numbers above). The Gitea tests (`TestMirrorHTTPSTokenRemote`, `TestMirrorSSHDeployKeyRemote`) skip without `TEAM_MEMORY_MIRROR_TEST_ENV`, so they skip in CI.
- New tests:
  - `internal/mirror`: document, go-git, Gitea remote and no-exec tests;
  - `integration/mirror_test.go`: 6 tests plus the benchmark;
  - `integration/security`: `TestTelemetrySampleWorkload`, `TestPurgeRunbookSQL`;
  - `internal/server` (`TestClientAddr`) and `internal/telemetry` (`TestRoute`).
- sqlc output is current. `go vet` is clean. `make helm-lint` and `make check-alerts` pass.
- `golangci-lint` (now installed locally) reports **13 issues, all in code from earlier stages**: unused helpers in `internal/portal`, unchecked `Close` errors, and two staticcheck suggestions. Findings in stage 06 code were fixed. The Makefile's `lint` target used to print "not installed" when lint *failed*; that is fixed, so `make lint` now fails until those 13 are cleaned up.

## What stage 07 needs

- **Hostname and URL placeholders**: `https://memory.example.com` (public URL), `/mcp`, `/portal`, `/oauth/callback/<id>`. For local testing the disposable cluster uses `https://memory.localtest.me` (trust `deploy/dev-k3d/.local/ca.pem`) and Keycloak at `https://keycloak.localtest.me/realms/team-memory-dev` (users and passwords in `deploy/dev-k3d/.local/env`).
- **Tokens for testing**:
  - solo/PAT: `tm-admin.sh <ns> <release> token create --principal <id>` (the k3d owner's id is in `deploy/dev-k3d/.local/init.out`);
  - OAuth clients sign in through Keycloak as `alice`;
  - a solo release without SSO (`tm-rollback` or `tm-load`) accepts PATs in the portal.
- `deploy/dev-k3d/setup.sh` recreates the cluster; `authentication.md` sections 2–7 install a release in about five minutes.
- Re-run Claude Code and Codex against the deployed endpoint; nothing in the MCP surface changed in this stage except the `X-Trace-Id` response header.
- The release checklist should rerun the license table in `git-library.md` and `make secret-scan`.
