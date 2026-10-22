# Threat model

Status: stage 04 (2026-10-21). Covers the MCP service, OAuth facade, database and worker as built through stage 04. The portal (stage 05) and the git mirror and deployment (stage 06) add surfaces that their stages must model; their requirements from this model are listed at the end.

## Assets

| Asset | Why it matters |
| --- | --- |
| Memory content and history | Team knowledge, possibly confidential (architecture, incidents, customers). Cross-tenant leakage tolerance is zero. |
| Trust and approval records | Distinguish reviewed guidance from proposals. Forgery launders an agent's guess into team policy. |
| Scope tree and memberships | Decide who reads and writes what. |
| Credentials | PATs, OAuth access and refresh tokens, signing keys, IdP client secrets, database passwords. |
| Audit log | Accountability and incident reconstruction. |
| Agents' behaviour | Retrieved memory is fed to coding agents with repository and shell access. |

## Actors

| Actor | Capability assumed |
| --- | --- |
| Anonymous network client | Reaches the HTTP endpoints. |
| Authenticated principal (human or agent) | Holds a valid credential with some scope roles; may be curious or malicious within them. |
| Compromised agent / prompt-injected agent | Acts with its principal's token, following attacker text. |
| Malicious writer | Can create memories other people's agents will read. |
| Rogue maintainer | Full rights in their scopes. |
| Upstream IdP or MCP client metadata host | Controls its own responses. |
| Attacker with SQL execution in `serve` | Arbitrary SQL as the app role (via a future injection bug). |
| Operator / database administrator | Trusted (owns data, runs migrations and the mirror). |

## Threats and mitigations

| # | Threat | Mitigations (where) | Tests |
| --- | --- | --- | --- |
| T1 | Cross-tenant or cross-scope read through any tool, resource, guessed ID, history, search (including vector) | Go scope checks; forced RLS on every tenant table with `scope_access`; not-found hides existence; scope filter before ranking | `TestHierarchyIsolationThroughMCP` (8 tools, 4 resources, 10 attacker positions, positive control), `TestCrossTenantRawSQL`, `TestEveryTenantTableHasForcedRLS` |
| T2 | Upward leakage: ancestor membership reveals descendants | Only cascading grants reach descendants; rank-0 rows are ancestors only | `TestInheritedMemoriesKeepOrigin`, `TestNonCascadingGrantsAndAdministration` |
| T3 | Writing or retiring guidance owned by a broader scope | Writer/maintainer on the owning scope only; `inherited_read_only` | `TestNoWritesAboveOwnScope`, `TestRolesPerAction` |
| T4 | Privilege escalation through administration (self-grant, cascading grant from a non-cascading maintainer, removing the owner) | `cascade_rank` rule; owner-grant trigger; RLS on `scope_members`; `scope_access` writable only by definer triggers; definer functions and guard triggers pin `search_path` with `pg_temp` last, so temporary tables cannot shadow the tables they trust | `TestNonCascadingGrantsAndAdministration`, `TestCrossTenantRawSQL`, `TestFunctionsIgnoreTempTables` |
| T5 | Agent launders a guess into policy (self-approval) | `memory_approve` only requests review; approval needs a human in a portal session; DB policy and guard trigger require it (the trigger's `search_path` cannot be shadowed by a temporary `approvals`); service principals never approve | `TestMCPCannotApprove`, `TestApprovalIsRevisionBound`, `TestFunctionsIgnoreTempTables` |
| T6 | Approval reused for changed content | Approval bound to a revision; every revision resets trust; stale revision is a conflict | `TestApprovalIsRevisionBound` |
| T7 | Override hides guidance without review, or reaches siblings | Links only to readable strict-ancestor memories; effective only with approval of the exact revision; rechecked per search over the reader's chain | `TestOverrides` |
| T8 | Promotion widens audience without consent, skips levels, leaks source history, or a destination maintainer grabs child memories | Two-sided browser consent; parent-only destination (policy); snapshot-only visibility; definer function rechecks everything; staleness detection; provenance and source-scope-name redaction | `TestPromotionConsentAndVisibility`, `TestPromotionStaleRejectedCancelled`, `TestPromoteDirect` |
| T9 | Stale access after revocation (caches, pooled connections) | Access recomputed in the revoking transaction; transaction-local identity; no access cache in Go | `TestRevocationIsImmediate`, `TestPoolReuseDoesNotLeakIdentity` |
| T10 | Missing or leaked request identity on a pooled connection | `set_config(..., true)` per transaction; NULL identity matches nothing; TxGuard asserts no statement outside a transaction | `TestMissingIdentitySeesNothing`, TxGuard in every integration test |
| T11 | History or audit tampering | Append-only triggers; app role lacks UPDATE/DELETE/TRUNCATE; audit rows only for own identity | `TestRevisionsImmutable`, `TestCrossTenantRawSQL`, `TestAuditTrail` |
| T12 | Token forgery or misuse: wrong audience, expired, forged, wrong issuer, ID token as access token, upstream IdP token, refresh replay | Facade-only JWT verification (issuer, audience, expiry, key, `token_use`); PATs hashed, revocable, expiring; disabled principals refused; refresh rotation with reuse detection (10 s race grace) | `TestAccessTokenValidation`, `TestOAuthRejectsUpstreamAndForeignTokens`, `TestRefresh*`, `TestRejectedCredentials` |
| T13 | Sign-in by unwanted users; account takeover by email match; arbitrary first login becoming owner | Per-provider issuer, verified email domain and group gates; identity keyed by (issuer, subject), never email; provisioned principals get no roles; owner only via `init`; linking needs both identities and a fresh upstream login | `TestOAuthPolicyRejections`, `TestOAuthEmailNeverLinks`, `TestOAuthProvisionedUserHasNoRoles`, `LinkFromPortal` |
| T14 | Unauthenticated access through a misconfiguration | No unauthenticated mode; SSO configured but broken leaves the server not ready (503 for everything) | `TestNoUnauthenticatedMode`, `TestSSOMisconfigurationFailsClosed` |
| T15 | Secrets stored in memory and spread to readers and the mirror | Write-time secret scan on all paths, rescanned at approval, promotion, and (stage 06) export | `TestSecretScanning`, `internal/secrets` tests |
| T16 | Prompt injection through stored memories | Untrusted-data notice first in every result and resource, plus `_meta` marker; server instructions; trust levels; agent policy; approval needed before guidance is authoritative | `TestUntrustedFraming` |
| T17 | Denial of service: large bodies, request floods, OAuth endpoint abuse, audit amplification | Body/header limits and timeouts; per-principal and per-address rate limits; capped pending state; rate-limited failure audit | `TestRequestLimits`, `internal/ratelimit` tests |
| T18 | SSRF through client metadata documents | Public-address-only fetcher, size and time limits, optional host allowlist | stage 03 tests |
| T19 | Mix-up and code injection in OAuth | PKCE S256 required; RFC 8707 resource binding; RFC 9207 `iss` in authorization responses; loopback-only redirects by default; per-client consent with CSRF | stage 03 tests, `addIssuer` |

## Residual risks

1. **Prompt injection is reduced, not eliminated.** Framing, trust levels and agent policy depend on the model and client honouring them. A memory can still mislead an agent that ignores the notice, and approved memories are trusted more, so a rogue or careless maintainer's approval is effective. Agents should verify against the repository; the policy says so but cannot enforce it.
2. **SQL execution in `serve` defeats RLS by impersonation.** The app role sets `app.principal_id` and `app.auth_method` itself (`TestIdentityVariableIsTheTrustBoundary`). Mitigated only by having no dynamic SQL and by the role's limited privileges. A compromised `serve` process can also forge audit rows attributed to any principal.
3. **The "browser review" boundary is a workflow marker,** not proof of a human: browser automation with a maintainer's session can approve. The portal's CSRF protection and session design (stage 05) determine its strength.
4. **Secret scanning is pattern-based.** Novel formats, split secrets and encoded secrets pass. Detection happens after the agent saw the secret.
5. **Identity tables are global.** The app role can list principals' display names and see token hashes (not tokens).
6. **Rate limits are per process** and keyed by the TCP peer address. Behind a proxy without trusted forwarding headers, OAuth limits are shared by all clients.
7. **Group-role sync trusts the IdP's group claim** at sign-in. Each synced grant records the issuer that justified it, and only a sign-in through that issuer changes or removes it (`TestIdPGroupSyncIsPerIssuer`). Removal at the IdP therefore takes effect at the next login through that IdP, or when an operator revokes the grant; a principal who stops using that IdP keeps its grants until then. When two IdPs map the same scope, the first grant keeps its owner and role until its IdP stops justifying it. Access tokens live up to 15 minutes, and refresh tokens re-run no sync.
8. **Refresh grace window:** within 10 s of rotation, a replayed refresh token is refused without revoking the family, so a thief racing the legitimate client inside that window does not trigger revocation.
9. **Signing keys reload only on restart** (documented rotation procedure). There is no automated key generation or rotation.
10. **Operators and database administrators are fully trusted,** including with mirror repositories.
11. **Single organization per deployment** for SSO providers; per-org provider configuration is deferred.
12. **No third-party review yet.** All tests here were written by the implementer.

## Requirements carried to later stages

- **Stage 05 (portal):** set `AuthMethod = portal` only for a cookie session with CSRF protection and re-authentication appropriate to review; sanitize rendered content (stored XSS) with a strict CSP; implement Connect SSO with `upstream.authCodeURL(..., forceLogin=true)` and `Identities.LinkFromPortal`; extend the hierarchy and cross-tenant tests to every portal API.
- **Stage 06 (operations):** export worker role and secret gate per `docs/decisions/mirror-security.md`; purge runbook; trusted proxy configuration for rate limits; Kubernetes Secrets for database login passwords; key rotation runbook.
- **Before public release:** third-party penetration test and code review of `migrations/00003_security.sql`, `internal/auth`, and the promotion definer function.
