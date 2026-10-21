# Handoff: stage 03 (identity provider decision)

Date: 2026-10-21. Branch: `claude/identity-provider-decision-a26adb` (worktree), based on the stage 02 commit `04808cc`. Committed on this branch; not pushed.

## Summary

**Architecture A is chosen:** Team Memory is a thin OAuth 2.1 authorization server for MCP clients, delegating login to customer-configured upstream OIDC providers and issuing its own short-lived tokens with audience `<origin>/mcp`. Personal access tokens remain for solo, CI and service use. Architecture B (validating upstream IdP tokens directly) is rejected and not implemented.

**Libraries:**
- `github.com/zitadel/oidc/v3` v3.51.16 (`op`) for the authorization-server protocol.
- `github.com/coreos/go-oidc/v3` v3.21.0 with `golang.org/x/oauth2` for the upstream login.
- go-jose v4 for verifying tokens.

The reasoning, weighted table, spec citations (MCP **2026-07-28**) and the Kubernetes contract are in `docs/decisions/identity-provider.md`. Real-client evidence is in `docs/verification/stage-03-oauth.md`.

**Both installed clients completed the full flow against the dev stack:** Claude Code 2.1.292 with Keycloak, and Codex 0.163.0 with Dex, the IdP that has no DCR. Each covered discovery, CIMD registration, consent, upstream login, token exchange, an authenticated `tools/call` and refresh. Both clients failed when pointed at the IdPs directly (architecture B).

## Acceptance criteria

| Criterion | Evidence | Result |
| --- | --- | --- |
| Decision record cites the spec version and distinguishes verified from assumed claims | `docs/decisions/identity-provider.md` (2026-07-28 URLs; a "verified vs read" table) | Pass |
| A real MCP client completes OAuth discovery and login against the dev stack and calls an authenticated tool | Claude Code: `memory_create` and `memory_search` as an SSO-provisioned principal. Codex: `memory_search` and `memory_get`, and `memory_create` → `permission_denied` as a reader. Re-run on the final build. | Pass (both clients) |
| Wrong audience, expired token or wrong issuer rejected (tests) | `TestAccessTokenValidation` (16 rejection cases including wrong/missing audience, expired, no `exp`, wrong issuer, unknown/forged key, `alg: none`, upstream-shaped token); `TestOAuthRejectsUpstreamAndForeignTokens` (HTTP 401 with challenge) | Pass |
| The record alone states architecture, library, why, and how an admin configures an upstream IdP via Kubernetes Secrets | Decision record: "Decision", "Library for A", "Kubernetes configuration contract" | Pass |
| Solo bootstrap works end to end with no IdP | `TestSoloBootstrapNoIdP`: `init`, the assembled server without SSO, no OAuth endpoints, and `memory_create` with the init PAT. Also `TestInitAndTokens` (stage 01) and the live `init` run on 2026-10-21. | Pass |
| SSO enabled with a missing or invalid secret: readiness fails clearly, and no unauthenticated access is possible (test) | `TestSSOMisconfigurationFailsClosed`: six cases, and `/readyz` 503 names the cause. `/mcp`, `/oauth/*` and `/.well-known/*` return 503 even with a valid PAT. `TestSSOSecretRemovedAtRuntime` covers removal after startup. | Pass |

`go test -race -count=1 ./...` against PostgreSQL 17 + pgvector passes locally. **CI has still never run** (nothing pushed).

## What was built

- `internal/auth/oauth/` (new, about 2,000 lines plus tests):
  - `config.go`: JSON config, validation, defaults.
  - `keys.go`: the signing keyring and rotation.
  - `store.go`: `op.Storage`, **in memory**, with PKCE enforcement, resource binding, rotating refresh tokens and reuse detection.
  - `clients.go`: DCR endpoint; CIMD fetcher with SSRF guard and cache; loopback-only redirects by default.
  - `upstream.go`: go-oidc discovery, exchange, ID-token checks, email-domain and group allow rules; the secret is reread on each use.
  - `login.go`: consent page with CSRF, browser-bound upstream state, and the callback that maps to a principal.
  - `tokens.go`: the access-token verifier.
  - `server.go`: wiring, RFC 9728/8414 metadata, RFC 8707 `invalid_target`.
- `internal/auth/identity.go`: `Identities.Resolve` (lookup or provision by `(issuer, subject)`; never by email; no roles) and `Identities.Link` (refuses identities linked elsewhere; idempotent; audited).
- `internal/auth/pat.go`: `auth.Chain` routes `tmpat_` credentials to PATs and everything else to OAuth. Adds `ErrInsufficientScope`.
- `internal/mcp/server.go`: the `ResourceMetadataURL` option. 401 and 403 carry `resource_metadata` and `scope="memory"`; insufficient scope gives 403.
- `internal/server/`: HTTP assembly used by `serve` and the tests. Readiness gates `/mcp`, `/oauth/` and `/.well-known/` while SSO is configured but unusable.
- `cmd/team-memory`: `serve` reads `TEAM_MEMORY_SSO_CONFIG` and `TEAM_MEMORY_PUBLIC_URL`. New `principal identities` and `principal link-identity` (operator recovery). Request logging covers all paths.
- Queries: `GetPrincipalByIdentity`, `InsertPrincipalIdentity`, `ListPrincipalIdentities` (sqlc regenerated). **No migration**: `principal_identities` already existed.
- `deploy/dev-idp/`: compose file (Keycloak 26.8.1, Dex v2.46.0, loopback only), realm and Dex config with environment placeholders, `setup.sh` generating throwaway credentials into git-ignored `.local/`, and a README. Make targets `dev-idp-up` and `dev-idp-down`.
- Tests: `internal/auth/oauth/tokens_test.go` (unit) and `integration/oauth_test.go`. The integration tests run the official go-sdk OAuth client against a fake OIDC provider without DCR, covering DCR and CIMD, policy rejections, email-never-links, the group gate, consent CSRF, foreign and upstream tokens, and PATs alongside SSO. Also `integration/sso_readiness_test.go`.

## Kubernetes config and Secret contract (summary)

The full table is in the decision record.

- **ConfigMap:** `sso.json`, with `issuer`, `signing_keys_dir`, `active_signing_key`, TTLs, `client_registration`, and `providers[]` (`id`, `issuer`, `client_id`, `client_secret_file`, `scopes`, `allowed_email_domains`, `allowed_groups`, `groups_claim`, `auto_provision`). Unknown fields are rejected, so secrets cannot be inlined.
- **Secret `team-memory-signing`:** `<kid>.pem` per key (P-256 preferred). All keys are published and accepted; the active key signs.
- **Secret `team-memory-idp-<id>`:** key `client-secret`, mounted without `subPath`, so rotation reaches the pod.
- **Register at each IdP:** a confidential client, redirect `<issuer>/oauth/callback/<id>`, code flow with PKCE S256.
- **Env:** `TEAM_MEMORY_SSO_CONFIG`, `TEAM_MEMORY_PUBLIC_URL`.
- **Readiness:** 503 with a reason until the config, keys and secrets are valid and discovery has succeeded for every provider. `/mcp` and the OAuth endpoints answer 503 meanwhile, with no fallback.

## Token-to-principal mapping stage 04 must implement

1. **At login:** verify the upstream ID token (signature, `iss` exactly the configured provider issuer, `aud` ∋ our client ID, `exp`, `nonce`). Apply the provider's allow rules: a domain rule requires `email_verified`; a group rule requires a listed group.
2. **Key:** `(id_token.iss, id_token.sub)`, looked up in `principal_identities (issuer, subject)` → `principal_id`. The principal must not be disabled.
3. **No match:** if `auto_provision`, create `principals(kind='human', display_name=name|email|"user <sub>")` and the identity row in one transaction, with no `scope_members` rows, and audit `auth.principal_provisioned` (issuer and subject only). Otherwise refuse with `access_denied`.
4. **Never:** match on email, assign ownership, or move an identity between principals.
5. **Team Memory access token (JWT):**
   - Claims: `iss` = deployment origin; `sub` = principal UUID; `aud` = [`<origin>/mcp`]; `exp` (default 15 min, max 1 h); `iat`/`nbf`; `jti`; `client_id`; `scope` (∋ `memory`); `idp` = upstream issuer, informational only.
   - Signature ES256 or RS256 with `kid`.
6. **Per request:** verify signature by `kid`, `iss` exact, `aud` ∋ resource, `exp` strict, `nbf` ±30 s, `scope` ∋ `memory`. Then load the principal by `sub`; it must exist and be enabled. Set `Principal.AuthMethod = "oauth"`. PATs keep `"pat"`.
7. **Refresh:** opaque `tmrt_…`, stored hashed, rotated on every use, family revoked on reuse (including two concurrent uses) and on RFC 7009 revocation (`TestRefreshRotationAndReuse`, `TestRefreshConcurrentReuse`, `TestRevokeRefreshToken`). Its lifetime is fixed at login (`refresh_token_ttl`); rotation does not extend it.

## Client-specific quirks found

The full list is in the verification note.

- Both clients choose **CIMD** in auto mode; neither needed flags. The CIMD URLs are `https://claude.ai/oauth/claude-code-client-metadata` and `https://chatgpt.com/oauth/codex/<id>/client.json`. Team Memory needs outbound HTTPS to fetch them.
- **Codex:**
  - redirect `http://127.0.0.1:<port>/callback/<id>`, where the registered URI has no port, so loopback matching must ignore the port;
  - an unauthenticated `GET /mcp` probe at 2024-11-05 before every session;
  - MCP at 2025-06-18 with `initialize`.
- **Claude Code:**
  - redirect `http://localhost:<port>/callback` and `prompt=consent`;
  - MCP at 2026-07-28;
  - four sequential refreshes within 12 s in one `-p` run;
  - after a server restart it stayed connected, because the JWT was still valid.
- Both send `resource=<origin>/mcp` and request `memory offline_access`.

## Unresolved items for stage 04

1. **Persist OAuth state in PostgreSQL.** Clients, pending authorizations, codes and refresh tokens are in memory now: a restart loses refresh tokens and DCR clients, and replicas cannot share them. This blocks running more than one replica and any real use.
2. **Refresh reuse grace window.** A concurrent duplicate refresh currently revokes the family.
3. **RFC 9207 `iss`** in authorization responses. Then advertise `authorization_response_iss_parameter_supported`, which lets Codex use its shared CIMD document and stable callbacks.
4. **Rate limits** on `/oauth/register`, `/oauth/authorize` and CIMD fetches. Audit events for sign-in success and failure and for denials.
5. **Signing-key hot reload**, or a documented restart, which is the current behavior. Possibly a `team-memory auth gen-key` helper.
6. **Portal Connect SSO linking** (stage 04 authorization, stage 05 UI), as specified in the decision record. It reuses `auth.Identities.Link` and must use a fresh upstream login (`prompt=login`) bound to the portal session.
7. **Decide on an audited `principal unlink-identity`** for operator recovery.
8. **Group-to-role mapping** is optional and later. Groups currently only gate sign-in.
9. **Multi-org deployments:** `init` allows one organization, and providers are not org-scoped.
10. **Distinguish ID tokens from access tokens.** zitadel sets a Team Memory ID token's `aud` to the MCP resource (from `GetAudience()`), so an ID token passes the signature, `iss`, `aud` and `exp` checks. It is refused at `/mcp` only because it has no `scope` claim (403 `insufficient_scope`). Add an explicit token-type check (for example a `typ`/`token_use` claim, or a separate audience for ID tokens) instead of relying on that.

## Skipped or not verified

- CI has not run (nothing pushed).
- **Real enterprise IdPs** (Entra ID, Okta, Google, Authentik) were not tested. Statements about their DCR, audience and claim behavior are from documentation and marked as read.
- **Pre-registered client flows** (`claude mcp add --client-id`, `codex mcp add --oauth-client-id`) were not exercised, and neither was architecture B with pre-registration.
- **SAML through a broker** is documented only.
- **The consent page** was exercised by a scripted browser (`urllib`) and by the go-sdk test client, not in a GUI browser. Its CSP (including `form-action` with upstream origins) has not been checked in Chrome, Safari or Firefox.
- **Not tested on real Kubernetes:** Secret rotation timing (kubelet refresh), readiness probes in a cluster, and multiple replicas. All three are stage 06 concerns, and the last needs stage 04 persistence first.
- **Local artifacts left in place for you:**
  - the dev IdP containers (`team-memory-dev-idp-*`), still running;
  - the throwaway database `teammemory_sso03`, holding two SSO-provisioned test principals and one test link to a fake Dex subject;
  - `deploy/dev-idp/.local/` (git-ignored).
  
  Client configuration entries and stored OAuth credentials created for the runs were removed.

## Re-estimate

Stage 03 took one session. The facade turned out smaller than feared, because both clients' CIMD support removes per-client setup and zitadel handles the protocol core. The largest remaining identity work in stage 04 is durable OAuth state (three tables plus cleanup jobs) and the rate-limit and audit plumbing. That is moderate next to RLS over the closure table. There is still no measured basis for a calendar estimate.
