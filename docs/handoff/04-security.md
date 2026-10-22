# Handoff: stage 04 (team security)

Date: 2026-10-21. Branch: `claude/team-security-prompts-50de41` (worktree), based on the stage 03 commit `f4e3fdf`. **Not committed or pushed yet.**

## Summary

Team Memory now runs every request under PostgreSQL row-level security as a non-owner role:

- **RLS:** forced on all 18 tenant-owned tables, driven by a trigger-maintained `scope_access` table.
- **Review:** revision-bound review requests, approvals and rejections. Approval and promotion complete only through trusted browser-review services, which MCP and bearer tokens can never reach.
- **Promotion:** two-sided consent, enforced through a narrow definer function.
- **Overrides:** approval-gated.
- **Audit and input defenses:** an audit log with request IDs, denials and authentication failures; secret scanning; request and rate limits; untrusted-data framing.
- **Production OAuth:** state in PostgreSQL, RFC 9207 `iss`, an ID-token/access-token distinction, a refresh grace window, per-address limits, sign-in audit, optional IdP group-to-role sync, and the portal SSO-linking authorization.
- **Adversarial suite:** `integration/security/`, 32 tests against real PostgreSQL.

`go test -race -count=1 ./...` passes locally (111 tests passed. The 2 env-gated benchmarks, `TestRetrievalEval` and `TestVectorIndexStrategies`, were skipped in that run and run separately for the RLS measurement. Generated sqlc code is current). **CI has still never run** (nothing pushed).

## Acceptance criteria

| Criterion | Evidence | Result |
| --- | --- | --- |
| Every tenant-owned table has RLS enabled and forced (automated, enumerates tables) | `TestEveryTenantTableHasForcedRLS`: every table in `public` must be forced-RLS or on an explicit non-tenant list (identity/OAuth tables, `embedding_models`, `goose_db_version`) | Pass |
| Adversarial suite passes; cross-tenant and hierarchy tests cover all eight tools and all four resources | `TestHierarchyIsolationThroughMCP`: 10 attacker positions × 8 tools (search, get, get-with-scope, history, recent, create, update, supersede, approve) × 4 resources, with a positive control that proves the probe detects access. Portal API coverage is added in stage 05. | Pass |
| `memory_approve` requests review without changing trust; trusted review binds approval to an exact revision; MCP/PAT calls, including a human maintainer's, cannot finalize approval or promotion | `TestMCPCannotApprove` (Go and SQL paths), `TestApprovalIsRevisionBound`, `TestPromotionConsentAndVisibility`, `TestPromoteDirect`; approved vs proposed overrides in `TestOverrides`. Browser end-to-end is stage 05. | Pass |
| Audit events exist for each sensitive action and cannot be altered by the app role | `TestAuditTrail` (create, update, review request, approve, supersede, three denial kinds, request IDs, no content); `TestRejectedCredentials` (authentication failures, no credential text); `TestCrossTenantRawSQL` (UPDATE, DELETE, TRUNCATE of audit refused); triggers still apply to the owner (`TestRevisionsImmutable`) | Pass |
| No unauthenticated mode exists in any configuration (test) | `TestNoUnauthenticatedMode` (solo, SSO missing, SSO broken, rate limit off; 7 malformed or absent credentials each) plus stage 03 `TestSSOMisconfigurationFailsClosed` and `TestSSOSecretRemovedAtRuntime` | Pass |
| Threat model lists residual risks, including injection limits | `docs/security/threat-model.md` (12 residual risks) | Done |

Prompt item 8, test by test:

| Required attack | Test(s) |
| --- | --- |
| Org-only member cannot reach team/project memories (search, get by ID, history, resources) | `TestHierarchyIsolationThroughMCP` (portal API: stage 05) |
| Project member cannot write or approve org/team memories | `TestNoWritesAboveOwnScope` |
| Team member cannot read a sibling team's projects | `TestHierarchyIsolationThroughMCP` ("sibling team's project", "another team's cascading member") |
| Non-cascading grant never reaches descendants | `TestHierarchyIsolationThroughMCP`, `TestNonCascadingGrantsAndAdministration` |
| Moving or re-parenting is impossible via any API | `TestScopesCannotMove` (app role SQL and owner), stage 01 `TestTreeRules`; no API offers it |
| Removed principal loses access immediately, including pooled state | `TestRevocationIsImmediate` (10 warmed pool connections; disabling a principal too) |
| Inherited memories show the right origin and cannot be read at an unreadable scope | `TestInheritedMemoriesKeepOrigin` |
| Deep-tree and many-member cases do not time out or leak | `TestDeepTreeManyMembers` (numbers below) |
| Agent cannot promote; writer without parent maintainer cannot; stale or unapproved revision fails; no level skipping; double submit idempotent | `TestPromotionConsentAndVisibility`, `TestPromoteDirect` |
| Override cannot reference an invisible or non-ancestor memory; no effect in sibling B or the team | `TestOverrides` (Go and SQL) |
| Promoted memory's former content not readable by the newly authorized beyond the intended audience | `TestPromotionConsentAndVisibility` (team history has 1 revision; source history and identifiers unreadable; provenance redacted) |
| Principal in X cannot read, search (including vector), get, history, embeddings or audit of Y | `TestHierarchyIsolationThroughMCP`, `TestCrossTenantRawSQL` (raw vector scan, embeddings, audit, jobs), `TestSearchUnderRLSUsesPolicyAndScopeFilter` |
| Reader cannot write; writer cannot approve or supersede | `TestRolesPerAction` |
| Connection-pool reuse does not leak identity (concurrent, interleaved) | `TestPoolReuseDoesNotLeakIdentity` (100 interleaved requests, 4 disjoint principals) and the TxGuard assertion in every integration test |
| Missing `app.principal_id` returns nothing | `TestMissingIdentitySeesNothing` (unset, nil UUID, outside a transaction; 15 tables) |
| Direct SQL under a fixed identity cannot evade policies or modify revisions/audit; impersonation documented | `TestCrossTenantRawSQL` (15 attack statements), `TestIdentityVariableIsTheTrustBoundary` (documents the residual) |
| Wrong-audience, expired, forged and revoked tokens rejected; non-allowed domain/issuer cannot sign in; upstream tokens not accepted | `TestAccessTokenValidation` (now 18 cases, including our own ID token), `TestOAuthRejectsUpstreamAndForeignTokens`, `TestOAuthPolicyRejections`, `TestRejectedCredentials` (revoked, expired, disabled) |
| Missing or invalid SSO secret leaves the server not ready | `TestSSOMisconfigurationFailsClosed`, `TestSSOSecretRemovedAtRuntime`, `TestNoUnauthenticatedMode` |

## What is enforced where

| Concern | Go | Database |
| --- | --- | --- |
| Visibility | `requireRole`, `visibleScopes`, not-found hiding | forced RLS via `scope_access` on every tenant table |
| Roles per tool | read: reader; create/update: writer; supersede, archive, `memory_approve`: maintainer | memories guard trigger; `memory_events` INSERT policy by event type |
| Approval, rejection | `Principal.browserReview()` (portal + human) + maintainer + current revision | `approvals` INSERT policy (portal, human, maintainer, current revision); guard requires an approval for `trust = approved` |
| Promotion | portal + maintainer on source (submit) / destination (finalize), rechecks, secret scan | `promotion_requests` policies and transition trigger; `tm_promotion_finalize_source` rechecks the contract; destination never gets source RLS access |
| Overrides | strict-ancestor check | `revision_overrides` INSERT policy |
| Administration | `CreateScopeAs`, `GrantMember`, `RevokeMember`, `ListMembers`, `AuditLog` | `scope_members`/`scopes` policies; `cascade_rank`; owner-grant trigger |
| History | no update paths | append-only triggers; no UPDATE/DELETE/TRUNCATE privileges |
| Identity per request | `WithActorTx`; TxGuard | `tm_principal()`/`tm_auth_method()`; NULL identity sees nothing |
| Roles of processes | `CheckRuntimeRole` (serve, worker), `CheckOperatorRole` (CLI) | group roles with least privilege |

The rules are documented in `docs/decisions/rls.md` and `docs/decisions/trust-rules.md`.

## What was built

- **Migrations:**
  - `00003_security.sql`: roles; `scope_access` with its triggers; consistency FKs (rows' `scope_id` must match their memory); `scope_id` on approvals and overrides; review-request idempotency; live-promotion uniqueness; `audit_events.request_id`; `idp_group_mappings`; guard triggers; two definer functions; RLS and policies; grants.
  - `00004_oauth_state.sql`: OAuth state tables.
  - `00005_idp_grant_issuer.sql`: IdP-synced grants record their issuer, so a sign-in through one provider changes only that provider's grants (found in review; `TestIdPGroupSyncIsPerIssuer`).
- **`internal/postgres`:** `Actor`, `WithActorTx`/`WithActorReadTx` (set principal and auth method), `CheckRuntimeRole`, `CheckOperatorRole`, `TxGuard`, `OpenWithTracer`.
- **`internal/memory`:**
  - `review.go`: `RequestReview`, `Approve`, `Reject`, `ReviewQueue`.
  - `promotion.go`: submit, finalize, reject, cancel, get, pending, direct.
  - `admin.go`: authorized scope and membership administration, `AuditLog`.
  - `audit.go`: request IDs, `InsertAudit`, denial audit.
  - `service.go`: actor transactions, maintainer supersede, secret scan, review info, override status, provenance redaction.
- **`internal/secrets`** (credential scanner) and **`internal/ratelimit`** (token buckets).
- **`internal/mcp`:**
  - `memory_approve` implemented; tool descriptions state roles;
  - results carrying memory content open with an `UNTRUSTED DATA` notice and set `_meta` `team-memory/untrusted-content`;
  - new error codes `secret_detected`, `browser_review_required`, `rate_limited`;
  - body limit, per-principal rate limit, authentication-failure hook.
- **`internal/server`:** request IDs (`X-Request-Id`), client address, failure auditor, limiter defaults.
- **`internal/auth`:**
  - `FailureAuditor` (rate-limited failure and sign-in audit);
  - `Identities.SyncGroupRoles`, `LinkFromPortal` (portal session + upstream login ≤ 5 min old), `Unlink`;
  - provisioning audit runs as the new principal.
- **`internal/auth/oauth`:**
  - `stateStore` with PostgreSQL (`pgState`) and memory (`memState`, unit tests) implementations, covering clients, requests, codes, refresh tokens and pending logins, all hashed except the 10-minute PKCE verifier and nonce;
  - 10 s refresh reuse grace;
  - `token_use=access` claim, now required;
  - RFC 9207 `iss` on authorization responses (and advertised);
  - per-address limits on the OAuth endpoints;
  - sign-in audit;
  - `sync_group_roles` provider option;
  - `prompt=login` support for linking.
- **CLI:**
  - `serve` requires `TEAM_MEMORY_APP_DATABASE_URL` and `worker` requires `TEAM_MEMORY_WORKER_DATABASE_URL`; each refuses unsafe roles;
  - `serve` HTTP timeouts and header cap (stage 01 open item);
  - new `db create-login`, `member revoke`, `memory archive --as`, `principal disable|enable|unlink-identity`, `idp-groups add|remove|list`, `audit list`.
- **Make targets:** `dev-roles`, `test-security`; `run` and `worker` use the dev login roles.
- **Tests:**
  - `integration/security/` (new);
  - every existing integration test now runs code under test as `tm_test_app`/`tm_test_worker` login roles (`internal/testdb.NewDB`), with fixtures on the admin pool;
  - the vector benchmark has an RLS mode (`TEAM_MEMORY_BENCH_RLS=1`).
- **Docs:** `docs/decisions/rls.md`, `trust-rules.md`, `mirror-security.md`; `docs/security/threat-model.md`, `limits.md`; `docs/agent-policy/AGENTS.md`, `CLAUDE.md`; README.

### Behaviour changes to know

- `memory_supersede` now needs **maintainer** (it was writer in stage 01), per the stage 04 prompt.
- `serve` no longer accepts `DATABASE_URL`. Run `make dev-roles` once locally.
- Tool results that carry memory content gained a leading `notice` field. The rest of the JSON is unchanged.
- `memory_get` gained `review`. Search and recent results gained `approved_by`/`approved_at`. Provenance links to memories the reader cannot see are now omitted.
- A concurrent duplicate refresh no longer ends the session (grace window). Reuse after 10 s still revokes the family.

## Measurements

Local Docker Desktop, PostgreSQL 17 + pgvector 0.8.1, Apple Silicon:

- **Deep tree** (`TestDeepTreeManyMembers`: 316 scopes, 16 levels, 2,000 principals, 4,000 grants, about 6,100 memories, 33,434 access rows):
  - full-text search from depth 15: 3.3 ms;
  - RLS `count(*)` as a member: 1.3 ms;
  - cascading root grant: 4.7 ms;
  - its revocation: 1.3 ms.
- **Vector search under RLS** (`make bench-vector` with `TEAM_MEMORY_BENCH_RLS=1`): at 100k embeddings, recall@50 is unchanged in every scenario (1.000 filtered, 0.997 unfiltered). Filtered p50 rises from 3.8–10.4 ms to 11.4–24.1 ms, and p95 stays at or below 25 ms, because the policy changes the plan to a hash join over `memories`. Details are in `docs/decisions/rls.md`; raw results are in `docs/eval/vectorbench-100k-rls.json`. Re-measure at target scale in stage 06.

## Config the Helm chart (stage 06) must expose

| Setting | Kind | Notes |
| --- | --- | --- |
| `DATABASE_URL` | Secret | operator role; used by a migration Job and admin tooling only, never by serve or worker |
| `TEAM_MEMORY_APP_DATABASE_URL` | Secret | login role in `team_memory_app` (`db create-login --role app`) |
| `TEAM_MEMORY_WORKER_DATABASE_URL` | Secret | login role in `team_memory_worker` |
| Group roles | DB bootstrap | migration creates them if the migrating role has `CREATEROLE`; otherwise a DBA pre-creates `team_memory_app`, `team_memory_worker`, `team_memory_operator` and grants the operator role to the owner |
| `TEAM_MEMORY_PUBLIC_URL` | ConfigMap | issuer and resource origin (`https://memory.example.com`) |
| `TEAM_MEMORY_SSO_CONFIG` | ConfigMap file | as in `docs/decisions/identity-provider.md`; new optional per-provider `sync_group_roles` |
| `team-memory-signing` | Secret | `<kid>.pem` per key; rotation needs a restart (documented in the identity decision) |
| `team-memory-idp-<id>` | Secret | `client-secret`, mounted without `subPath` |
| `TEAM_MEMORY_RATE_LIMIT`, `TEAM_MEMORY_RATE_BURST` | ConfigMap | per principal, per replica |
| Trusted proxy configuration | missing | rate limits key on the TCP peer; behind an ingress, add a trusted `X-Forwarded-For` setting or limit at the ingress |
| Readiness | probe | `/readyz` (database + SSO); `/healthz` liveness |

Replicas can now share OAuth state. Rate limits remain per replica.

## Residual risks and recommended external review

See `docs/security/threat-model.md` for all of them. The most important:

1. A SQL-injection bug in `serve` could impersonate any principal, because the app role sets its own identity. RLS does not defend against that.
2. Prompt injection is reduced, not eliminated.
3. The "browser review" marker is a workflow boundary, not proof of a human.
4. Secret scanning is pattern-based.

**Recommended before public release:** an independent penetration test and security code review. Focus on:

- `migrations/00003_security.sql` (policies, guard triggers, both definer functions);
- `internal/auth/oauth` (state persistence, refresh rotation, consent and CSRF);
- the portal session design that stage 05 adds.

Third-party testing is outside this prompt and was not done.

## Skipped, not verified, or deferred

- **CI has not run** (nothing pushed).
- **No real MCP client was re-run against stage 04.** The go-sdk client tests cover the protocol, but Claude Code and Codex have not seen the leading `notice` field, the new error codes, or `iss` in authorization responses. Stage 07 verifies clients, and a quick re-run before then is advisable.
- **Signing-key hot reload** was not built. Restart is the documented procedure, which the prompt allows.
- **Portal parts are stage 05:** the browser review UI and the Connect SSO browser flow. Stage 04 supplies and tests the service methods and authorization. There is no HTTP admin API besides the operator CLI. The authorized service methods are what the portal will call.
- **Multi-organization SSO** (per-org providers) is still deferred. One organization per deployment.
- **Locked organization guidance** is deferred, per the prompt.
- **`idempotency_keys`** is RLS-protected but still unused.
- **Real enterprise IdPs** remain untested (as in stage 03), including group claims for `sync_group_roles`.
- **The benchmark ran on Docker Desktop,** not the Kubernetes target.
- **Third-party penetration testing** was not done (see above).
- **Open question: revoking cascading grants.** A maintainer of a scope holding only a non-cascading grant can revoke or downgrade someone's cascading grant on that scope, which also covers child scopes the maintainer doesn't administer. Neither `review-decisions.md` nor the prompt decides this; it is allowed for now. If it should not be, require `cascade_rank` 3 to change a cascading grant, as granting one already does.
