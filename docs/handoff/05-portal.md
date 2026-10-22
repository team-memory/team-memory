# Handoff: stage 05 (web portal)

Date: 2026-10-22. Based on the stage 04 commit `d8cfb59`. **Not committed or pushed.** CI has still never run.

## Summary

A server-rendered web portal ships inside `team-memory serve` (`internal/portal`, embedded templates and CSS, no JavaScript, no Node build). It browses scopes, shows own and inherited memories (never descendants'), searches (same service as MCP), shows provenance, history and diffs, and lets maintainers review: approve, reject, archive, supersede, promote, accept promotion requests, and writers mark overrides. Browser sign-in works with personal access tokens (solo) and the configured upstream OIDC provider. Connect SSO links an identity to the signed-in account. Decision record: `docs/decisions/portal.md`. User guide: `docs/portal.md`.

`go test -race -count=1 ./...` with `TEAM_MEMORY_E2E=1`: 215 passed, 2 skipped (the env-gated `TestRetrievalEval` and `TestVectorIndexStrategies`), 0 failed, against PostgreSQL 17 + pgvector on Docker Desktop. The independent review and its fixes are listed at the end.

## Acceptance criteria

| Criterion | Evidence | Result |
| --- | --- | --- |
| Project reader sees project memories plus labeled inherited team and org memories, no descendants; org-only sees only org; cascading org admin sees everything below | `TestPortalHierarchyVisibility`: ten positions (reader, writer, maintainer, org-only, cascading org admin, sibling team, sibling project, nobody) × every scope, own and inherited tabs, navigator | Pass |
| No view, count, search result, error or autocomplete reveals a scope or memory the principal cannot read | `TestPortal404IsIndistinguishable` (byte-identical 404s modulo request id), `TestPortalEndpointsHierarchy` (10 positions × all scopes × GET/POST endpoints, state unchanged, positive control), `TestPortalRemovedMemberLosesAccessOnNextRequest` | Pass |
| Promotion: team reader can read it, project members see it inherited, no duplicate; without destination authority it lands in the team queue | `TestPortalPromotion` (one-step, request then accept, reader/writer refused, stale, revoked consent, double submit) | Pass |
| Approved override collapses the inherited memory in project views, org view unchanged | `TestPortalOverrides` (draft hides nothing; approval activates here and below; sibling and org unaffected; update and archive disable it; toggle reveals) | Pass |
| Maintainer approves in the portal; MCP shows `approved` with the approver; audit records it | `TestPortalApproveFlow`, `TestPortalBrowserEndToEnd` | Pass |
| XSS, CSRF and session tests pass; CSP enforced in the browser | `TestMarkdownNeutralizesHostileInput`, `TestPortalStoredXSS`, `TestPortalCSRF`, `TestPortalSessions`; Playwright refuses an injected inline script and reports a `script-src` violation | Pass |
| Works in solo mode and with the SSO upstream | Solo: every `integration/security` portal test. SSO: `TestPortalSSOSignIn`, `TestPortalConnectSSO` against the fake OIDC provider | Pass for the fake provider. **Not re-run against the Keycloak dev stack** (see gaps) |
| All tests pass against real PostgreSQL; skipped checks listed | above | Pass; skips listed below |

Prompt item 7 test list: hierarchy (done), XSS and CSP and CSRF for every POST (done), fixation, expiry, logout, cross-user reuse (done), SSO linking including conflicts, email-only, CSRF and replayed callbacks (done), 403/404 (done), browser end-to-end (sign in, navigate, inherited, search, history, approve, audit), promotion cases, overrides, extended adversarial suite (done).

## What was built

- `migrations/00006_portal.sql`: `portal_sessions` and `portal_login_states` (hashed ids, identity tables outside RLS; classified in `TestEveryTenantTableHasForcedRLS`). sqlc models regenerated.
- `internal/portal`: `portal.go` (mount, headers, session and CSRF plumbing), `session.go`, `auth.go` (token and SSO sign-in, Connect SSO, account), `browse.go` (navigator, scope page, search, members), `review.go` (memory page, approve, reject, archive, supersede, promote, override, queue, promotion requests), `render.go` (escape-first markdown), `diff.go`, templates, CSS.
- `internal/memory/browse.go`: `ScopePage` (filters, grouping, override map), `ConflictsFor`, `SecretScan`, `EffectiveRoleOn`, `DisplayNames`. Single read-only transactions, so RLS decides every row and count.
- `internal/auth/oauth/portal.go`: hooks so the portal shares the facade's providers and callback URL. `internal/auth`: `Identities.List`.
- `internal/server`: mounts the portal behind the same readiness gate as `/mcp`; `cmd/team-memory`: `TEAM_MEMORY_PORTAL_*` settings.
- Tests: `integration/security/portal_*_test.go`, `integration/portal_sso_test.go`, `e2e/` (Playwright), `internal/portal/render_test.go`. `make test-e2e`.

## Behaviour changes to know

- `Referrer-Policy` is now `same-origin` on the portal **and the stage 03 OAuth consent page**. With `no-referrer`, Chromium sends `Origin: null` on same-origin form posts, and the consent page's `Origin` check would have refused every real browser (found by the Playwright run; stage 03 had only exercised that page with scripted clients).
- With SSO configured, token sign-in is **closed by default** (`TEAM_MEMORY_PORTAL_TOKEN_SIGNIN=auto`); `owner` opens it for the organization owner until they link an identity. Unknown portal settings now stop startup.
- `/readyz` and `/healthz` are unaffected by portal failures; a portal that fails to build logs an error and is not mounted.

## Config the Helm chart (stage 06) must expose

| Setting | Kind | Notes |
| --- | --- | --- |
| `TEAM_MEMORY_PORTAL` | ConfigMap | `off` disables |
| `TEAM_MEMORY_PORTAL_BASE_PATH` | ConfigMap | default `/portal`; `/` for a dedicated hostname. Ingress must route this path and `/oauth/callback/*` |
| `TEAM_MEMORY_PUBLIC_URL` | ConfigMap | must be the exact browser origin (https in production): drives `Secure` cookies and the `Origin` check |
| `TEAM_MEMORY_PORTAL_IDLE_TIMEOUT`, `_ABSOLUTE_TIMEOUT` | ConfigMap | defaults 30m, 12h |
| `TEAM_MEMORY_PORTAL_SOURCE_SCHEMES` | ConfigMap | extra link schemes |
| `TEAM_MEMORY_PORTAL_TOKEN_SIGNIN` | ConfigMap | `auto` (closed with SSO), `owner`, `all`, `off`. A fresh SSO deployment needs `owner` once, to Connect SSO |
| `TEAM_MEMORY_PORTAL_CSP_EXTRA` | ConfigMap | appended CSP directives |
| Session secret | none needed | sessions and CSRF tokens are random values in PostgreSQL; no signing secret |
| Cookie settings | fixed | `HttpOnly`, `SameSite=Lax`, `Secure` when the public URL is https, `__Secure-` prefix |
| IdP redirect URI | per provider | unchanged: `<issuer>/oauth/callback/<id>` serves both MCP and portal sign-in |
| Trusted proxy | missing | sign-in and session rate limits key on the TCP peer (see gaps) |

Static files are served by the app at `<base>/static/portal.css`; an ingress CDN is not required.

## Measure in stage 06

Portal page latency under load: scope page with 100 memories (one read transaction plus name lookups), memory page (history plus conflicts, about 10 queries), review queue (runs `ConflictsFor` for up to 25 items), and session lookup cost (one write-capable transaction per request, touching `last_seen_at` at most once a minute). No numbers were measured in this stage.

## Known gaps and skipped checks

- **Keycloak not re-run.** The SSO tests use the in-process fake OIDC provider. The Keycloak and Dex dev stacks were not started, so real provider behaviour for `prompt=login`, `max_age=0` and the `auth_time` claim is untested. If a provider omits `auth_time`, Connect SSO fails with a "did not confirm a fresh sign-in" message by design; the fake provider test covers that path. Run `make dev-idp-up` and a manual Connect SSO before relying on it.
- **Chromium only** for browser tests; no Firefox or Safari, no screenshots generated. No automated accessibility audit; keyboard and phone-width checks are two Playwright assertions, and dark mode is CSS only, unchecked in a browser.
- **Playwright is not in CI** (CI never ran; `make test-e2e` needs `npm ci` and a Chromium download). The Go launcher skips without `TEAM_MEMORY_E2E=1`.
- **Conflict candidates** rely on the stage 02 worker. The tests insert a candidate row directly; the portal reads whatever the worker detected. There is no live "similar to a memory in another scope" check at promotion time beyond those rows (the prompt's wording for review of a highly similar memory is met through the stored candidates).
- **Audience impact** shows the destination path, the fact that all scopes below inherit, and the destination's direct member count only for maintainers there. No count of descendant scopes or readers is shown.
- **Promotion links** appear only where the service returns them (it redacts unreadable targets); there is no separate "redacted" marker text.
- **No step-up re-authentication** for approvals (threat model, residual risk 3).
- **Rate limits** are per process; sign-in limits are per TCP peer, so behind an ingress they are shared.
- **Supersede in the portal** requires entering replacement text, because there is no other way to name a replacement; it is a small form, not a general editor.
- **Search page** has no date or status filters (scope pages do); `include superseded` is accepted as a parameter but has no checkbox. Search shows a similar inherited memory under a result only from stored conflict candidates (worker-detected), not computed live.
- The browser end-to-end test checks the audit event from Go after the browser run, not in the browser; the portal has no audit view.
- A `PromoteDirect` whose acceptance step is refused after submission (authority lost in between) falls back to showing the filed request, but the two steps are separate transactions in `internal/memory`.
- Deferred, as in the prompt: editing, member and scope administration UI, notifications, analytics, re-parenting.

## Independent review (Opus 5.5 subagent) and fixes

The review reported 2 high, 6 medium and 5 low defects plus test-quality and disclosure gaps. All were fixed or are disclosed above; the main boundaries (escaping, CSRF, session rotation and revocation, login-state binding, 404 uniformity, revision binding, fail-closed SSO) were checked and found sound.

| # | Finding | Fix | Test |
| --- | --- | --- | --- |
| 1 (high) | Every inherited card showed a fake "Overridden at" note (zero struct is truthy in templates) | Pointer map; note only when an override exists | `TestPortalOverrides` asserts a plain inherited memory has no note, exactly one note when overridden |
| 2 (high) | With SSO, any human PAT holder without a linked identity could sign in to the portal and get review authority | Default closed with SSO; `owner` mode limited to the organization owner; strict validation of the setting | `TestPortalTokenSignInDefaultsClosedWithSSO`, `TestPortalConnectSSO`, `TestPortalConfigValidation` |
| 3 | `next` open redirect with a root base path (tab); dot-segment escape | Reject control characters, schemes, hosts, dot segments | `TestPortalRedirectTargets` (both base paths) |
| 4 | One limit across own and inherited hid own memories | Separate queries and limits; probe row dropped | `TestPortalScopePageLimitsAreSeparate` |
| 5 | Override collapse depended on the overrider being in the filtered list | Service returns the referenced memories; collapse independent of filters | `TestPortalOverrides` (kind filter) |
| 6 | Promotion double submit gave an error | Repeat lands on the result | `TestPortalPromotion` (asserts status and Location) |
| 7 | `dest_maintainer` form field trusted; failed finalize hid a created request | Server decides from the database; fall back to the request page | forged-field subtest |
| 8 | Search links double-encoded the scope | Removed manual escaping | covered by the search tests |
| 9 | Unbounded diff work per page view | Diffs for the newest 20 revisions only | none (bound by construction) |
| 10 | Navigator indentation used inline style blocked by the CSP | CSS classes | `TestPortalScopePageLimitsAreSeparate` |
| 11 | Connect SSO freshness measured against now | Also requires login after the link was started | stale path covered by existing test |
| 12 | Portal settings failed silently | Validation at startup | `TestPortalConfigValidation` |
| 13, 14, 15 | Vacuous override assertions; cross-browser callback test did not test binding; missing one-step, audience-impact and `Origin: null` tests | Rewritten or added | `TestPortalOverrides`, `TestPortalSSOSignIn`, `TestPortalPromotion`, `TestPortalCSRF` |
| 16 | Search showed no conflict candidates | Similar inherited memory shown under results | none beyond rendering |
| 17 | Untrue handoff claims | Corrected in this note |  |

Not fixed: a recorded separate-transactions note for `PromoteDirect` (above) and low-risk PLAUSIBLE items already listed under gaps.
