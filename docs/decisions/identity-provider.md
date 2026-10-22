# Decision: identity provider and authentication architecture

Status: accepted (stage 03, 2026-10-21). Production hardening of the selected architecture is stage 04's work.

## Decision

1. **Architecture A: Team Memory is a thin OAuth 2.1 authorization server (a "facade") in front of any customer-configured upstream OpenID Connect provider.** MCP clients discover Team Memory as their authorization server and register with it, by Client ID Metadata Document or dynamic registration. Users approve the client on a Team Memory consent page and sign in at the upstream IdP. Team Memory is a pre-registered confidential client of that IdP. After the login, Team Memory issues its **own** short-lived JWT access tokens, with `iss` set to the deployment origin and `aud` set to the canonical MCP resource URI (`https://<host>/mcp`). Refresh tokens are opaque and rotating. Upstream tokens are never accepted at `/mcp` and never forwarded.
2. **Personal access tokens stay** for solo use, CI and service accounts, in every mode.
3. **Architecture B (resource-server-only, validating tokens issued directly by the customer's IdP) is rejected for v1** and is not implemented as an optional mode.
4. **Libraries:**
   - [`github.com/zitadel/oidc/v3`](https://github.com/zitadel/oidc) v3.51.16, package `op`, implements the authorization-server protocol: the authorize and token endpoints, PKCE verification, authorization codes, the refresh grant, JWKS and revocation.
   - [`github.com/coreos/go-oidc/v3`](https://github.com/coreos/go-oidc) v3.21.0 plus `golang.org/x/oauth2` v0.37.0 implement the upstream relying party: discovery, code exchange with PKCE, and ID-token verification.
   - [`github.com/go-jose/go-jose/v4`](https://github.com/go-jose/go-jose) verifies Team Memory access tokens.
   - All are Apache-2.0.
5. **Principal mapping:** an upstream identity is `(issuer, subject)` → `principal_identities` → `principals.id`. The Team Memory token's `sub` is the principal UUID. Email is never a lookup key.
6. **Roles are database-managed** (`scope_members`). IdP group claims may *gate sign-in* per provider (`allowed_groups`) but do not assign roles in v1. Group-to-role mapping is a later, optional feature (see "Roles" below).

## Specification checked

MCP specification **revision 2026-07-28**, read on 2026-10-21:

- [Authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
- [Authorization server discovery](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/authorization-server-discovery)
- [Client registration](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/client-registration)
- [Security considerations](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/security-considerations)

The requirements that shaped the design:

| Requirement (2026-07-28) | How Team Memory meets it |
| --- | --- |
| MCP servers MUST implement RFC 9728 protected-resource metadata; clients use `WWW-Authenticate: resource_metadata` or probe `/.well-known/oauth-protected-resource/<path>` and then the root | Both URLs are served. 401 and 403 responses carry `resource_metadata` and `scope="memory"`. |
| Authorization servers MUST offer RFC 8414 or OIDC discovery; clients try `oauth-authorization-server` first | The same document is served at `/.well-known/oauth-authorization-server` and `/.well-known/openid-configuration`. |
| Clients MUST refuse an AS whose metadata lacks `code_challenge_methods_supported`; S256 required | Advertises `["S256"]`. The AS rejects any request without an S256 challenge. Upstreams that do not advertise S256 fail discovery. |
| Client ID Metadata Documents SHOULD be supported. DCR is **deprecated**, kept as a MAY for compatibility. Pre-registration is also allowed. | CIMD and DCR are both on by default. DCR registers public clients only. |
| AS MUST fetch the CIMD, check `client_id` equals the URL, validate redirect URIs against it, and SHOULD guard against SSRF | Done (`clients.go`): https only, a non-public-address dialer (also refusing 100.64/10 and the NAT64, 6to4 and Teredo prefixes that embed an IPv4 address), no redirects, 64 KiB cap, 5 s timeout, cache bounded to 1 min–24 h. An optional host allowlist is available. |
| Clients MUST send RFC 8707 `resource`. Servers MUST validate that tokens were issued for them and MUST NOT accept or pass through other tokens. | `resource` must equal the canonical `/mcp` URI, compared case-insensitively for scheme and host and tolerating a trailing slash. Otherwise the AS answers `invalid_target`. Tokens carry `aud=[<origin>/mcp]`, and the verifier requires it. |
| Proxy servers with a static upstream client ID MUST obtain user consent for each dynamically registered client | A consent page names the client, its client ID, the redirect host (with a warning for loopback redirects) and the resource, before any upstream redirect. |
| AS endpoints MUST be HTTPS; redirect URIs localhost or HTTPS | The issuer must be https. `allow_insecure_http` permits http only on a loopback host, for development. DCR and CIMD accept only loopback-http redirect URIs by default; https redirects to other hosts need `allow_remote_redirect_uris`. |
| Short-lived access tokens; public-client refresh tokens MUST rotate | Access tokens default to 15 min (max 1 h). Refresh rotates on every use, and replaying a rotated token, sequentially or concurrently, revokes its family. RFC 7009 revocation of a refresh token revokes its family. Authorization codes are single use, also under concurrent redemption. |
| Authorization responses SHOULD include RFC 9207 `iss` | **Not done.** zitadel/oidc does not emit it, so Team Memory does not advertise it. Both clients accept the response without it, as the spec's "absent/absent → proceed" row allows. Codex then uses per-server CIMD documents and callback IDs instead of its shared ones. A later version should add it. |

Codex 0.163.0 speaks protocol 2025-06-18, which already required PRM, RFC 8707 and PKCE; DCR was a SHOULD in that revision, and CIMD postdates it. Team Memory's metadata satisfies both revisions.

## Client behavior: verified vs read

Installed versions: **Claude Code 2.1.292**, **Codex CLI 0.163.0** (`claude --version`, `codex --version` on a macOS workstation, 2026-10-21). Full log: `docs/verification/stage-03-oauth.md`.

| Behavior | Claude Code 2.1.292 | Codex 0.163.0 | Source |
| --- | --- | --- | --- |
| Discovery | `POST /mcp` → 401, then the PRM path-insertion URL, then `oauth-authorization-server` | Unauthenticated `GET /mcp` (protocol 2024-11-05) → 401, then the same two documents | **Run** (server log) |
| Registration chosen in auto mode | CIMD, `client_id=https://claude.ai/oauth/claude-code-client-metadata` | CIMD, per-server `https://chatgpt.com/oauth/codex/<id>/client.json` | **Run** |
| Redirect URI | `http://localhost:<port>/callback` | `http://127.0.0.1:<port>/callback/<id>`; the registered URI has no port, and the AS must allow the varying loopback port (RFC 8252 §7.3) | **Run** |
| Scope requested | `memory offline_access` (`memory` from the challenge; `offline_access` because the AS lists it) | `memory offline_access` | **Run** |
| `resource` sent | `http://localhost:8080/mcp` on authorize | `http://localhost:8080/mcp` on authorize | **Run** |
| Login, token exchange, authenticated `tools/call` | Pass (Keycloak upstream) | Pass (Dex upstream) | **Run** |
| Refresh after access-token expiry (1-min tokens) | Refreshed, then `tools/call` succeeded. Made four sequential refreshes within 12 s for one `-p` run (one per internal connection). | One refresh, then `tools/call` succeeded | **Run** |
| Fails when PRM names an IdP with no DCR/CIMD (architecture B, Dex) | "Incompatible auth server: does not support dynamic client registration" | "Dynamic client registration not supported" | **Run** |
| Fails when PRM names default Keycloak (architecture B) | DCR 403 "Allowed Client Scopes" policy | Same 403 | **Run** |
| Non-interactive `-p`/`exec` runs reuse the stored OAuth sign-in | Yes | Yes | **Run** |
| `--client-id` / `--oauth-client-id` pre-registration, `--callback-port`, `authServerMetadataUrl`, `oauth.scopes` | Documented | Documented (`--oauth-client-id`, `--oauth-client-secret`, `--oauth-resource`) | **Read** ([Claude Code MCP docs](https://code.claude.com/docs/en/mcp), [Codex MCP docs](https://learn.chatgpt.com/docs/extend/mcp?surface=cli), `--help` output) |
| Token endpoint only over HTTPS or loopback | Documented | Not stated | **Read** |

## Architectures compared

Weights run 1 to 5. Scores run 1 to 5, where 5 is best for Team Memory.

| Criterion | Weight | A: facade + upstream OIDC | B: resource server only |
| --- | --- | --- | --- |
| Works with IdPs lacking DCR/CIMD (Dex, Google, Entra, most Okta tenants) | 5 | **5**: only Team Memory needs a client at the IdP, created once by an admin. **Run** with Dex. | **1**: both clients fail outright without per-client pre-registration. **Run** with Dex and default Keycloak. |
| Audience handling (tokens bound to this resource) | 5 | **5**: Team Memory mints `aud=<origin>/mcp` itself and rejects everything else. | **2**: depends on each IdP. Dex sets `aud` to the client ID (**run**: discovery shows no resource-indicator support). Entra binds to an App ID URI, Google issues opaque access tokens, and Keycloak needs audience mappers (**read**, not tested). Each IdP needs its own validation rules. |
| Client compatibility with zero per-client setup | 4 | **5**: both clients completed login with no flags. | **2**: works only with pre-registration (`--client-id`, fixed `--callback-port`) per client per IdP, if the IdP accepts loopback redirects (**read**). |
| Solo-mode simplicity | 3 | **4**: PATs unchanged. SSO adds one JSON file, a signing key and a secret, all optional. | **4**: PATs unchanged. |
| Security-sensitive code we own | 4 | **2**: consent, CIMD fetching (SSRF), DCR, refresh storage, principal mapping and the upstream bridge are ours. The protocol state machine is the library's. | **4**: JWT/JWKS validation per issuer only. |
| Operational weight | 2 | **3**: a signing key, one secret per provider, and durable OAuth state (stage 04). | **4**: issuer and audience configuration per IdP. |
| **Weighted total (max 115)** | | **96** | **59** |

A wins on the two criteria that decide whether SSO works at all. B's advantages are real but smaller: less code, and no token minting. B also cannot deliver "bring any OIDC provider", which the proposal requires. Its one real use case is an organization whose IdP supports CIMD and RFC 8707 audiences, and that IdP can sit behind the facade instead. **B is not implemented.** Stage 04 must not accept upstream tokens at `/mcp` (tested: `TestOAuthRejectsUpstreamAndForeignTokens`).

### Library for A

| Option | License | Maintenance (GitHub, 2026-10-21) | Fit | Verdict |
| --- | --- | --- | --- | --- |
| `zitadel/oidc/v3` `op` | Apache-2.0 | v3.51.16 released 2026-10-15; pushed 2026-10-19 | AS core: authorize/token, PKCE, codes, refresh, JWKS, revocation. Loopback-port-agnostic redirect matching for native clients. Pluggable storage. Used in production by the ZITADEL identity platform. Its **RP** is OpenID-certified; the **OP is not certified** (it is on the README's roadmap). No DCR, CIMD, RFC 8707 or RFC 9207, so those are ours. | **Chosen** |
| `ory/fosite` | Apache-2.0 | last release v0.49.0 on 2024-12-12; last push 2025-11-20 | Mature, used by Ory Hydra. Nearly two years without a release. Also no CIMD. | Rejected: maintenance risk |
| Run Ory Hydra, Keycloak or Dex as the facade | Apache-2.0 | active | Hydra: an extra service plus a login/consent app. Keycloak: heavy for a solo install, and CIMD is a recent feature behind configuration (26.6+, **read** from a secondary source, not checked). Dex: no DCR or CIMD (**run**). | Rejected for the default. Keycloak/Dex remain the documented SAML broker. |
| go-sdk `auth`/`oauthex` | MIT | v1.8.0 | Resource-server middleware, metadata types and an OAuth *client*. No AS. | Used for the bearer middleware and in tests |
| Hand-rolled on `x/oauth2` + go-jose | n/a | n/a | n/a | Rejected (prompt rule: do not hand-roll the protocol) |

Upstream RP: `coreos/go-oidc` v3.21.0 (released 2026-09-01) rather than zitadel's `rp`. It is the most widely deployed Go ID-token verifier, and it keeps the upstream path small.

## Authentication modes (one `Authenticator` interface)

`auth.Chain` routes credentials with the `tmpat_` prefix to PATs and everything else to the OAuth verifier when SSO is enabled. With SSO off, only PATs are accepted.

| Mode | Credential | Verified by | `Principal.AuthMethod` |
| --- | --- | --- | --- |
| Personal access token | `tmpat_…` bearer. 256-bit random, SHA-256 at rest, expiring (default 90 d, max 366 d), revocable, shown once. | `auth.PATs` (stage 01) | `pat` |
| Upstream OIDC login via the facade | Team Memory JWT (ES256/RS256) | `oauth.AccessTokens`: signature by a configured key, `iss` exact, `aud` ∋ resource, `exp` strict, `nbf` ±30 s, `scope` ∋ `memory`, `sub` an enabled principal (checked in the DB per request) | `oauth` |
| Direct validation (B) | n/a | **not implemented** | n/a |

PAT scopes: a PAT currently grants the `memory` scope implicitly. Role checks still apply. Stage 04 adds per-token scopes, such as read-only or CI tokens, if needed.

Disabling a principal (`principals.disabled_at`) stops both PATs and OAuth tokens on the next request, because both paths read the principal row. An upstream account deprovisioned at the IdP keeps working until its Team Memory refresh token expires (default 30 d) unless the principal is disabled. SCIM is out of scope. Stage 04 should consider a shorter `refresh_token_ttl` for strict tenants.

## Solo bootstrap and later SSO

Implemented as decided in `review-decisions.md` (not reopened):

1. `team-memory init --org <slug> --owner <name>` creates the organization, a default project, the owner principal with a cascading maintainer grant, and the first PAT, in one transaction. A rerun refuses with `already initialized` and never changes `org_settings.owner_principal_id` (`TestInitAndTokens`). No IdP is needed: `TestSoloBootstrapNoIdP` runs init, starts the assembled server without SSO, checks that the OAuth endpoints do not exist, and calls `memory_create` with the init token.
2. To add SSO later, the operator supplies the SSO config and Secrets (contract below) and restarts. Existing principals and PATs are untouched.
3. **Linking the existing owner to their SSO identity (portal flow, stage 04 authorization, stage 05 UI):**
   1. The owner is signed into the portal with an existing credential (PAT sign-in for a solo owner).
   2. They choose **Connect SSO** (a CSRF-protected POST) and pick a provider.
   3. Team Memory starts a *fresh* upstream login with `prompt=login` and a state/nonce bound to that portal session and to a linking intent.
   4. On callback, Team Memory verifies the ID token as in a normal login.
   5. It calls `auth.Identities.Link(principal, {issuer, subject})`. That refuses an identity already linked to a different principal (`ErrIdentityLinked`), is a no-op if already linked to this principal, refuses disabled principals, and writes an `auth.identity_linked` audit event.
   6. Ownership is not touched. `org_settings.owner_principal_id` stays the same principal.
4. **Invariants (tested):**
   - Email equality never links. `TestOAuthEmailNeverLinks` signs in an upstream identity whose email matches an existing principal's linked email: it becomes a separate principal.
   - A first SSO login never confers ownership or roles. `TestOAuthProvisionedUserHasNoRoles` checks that the owner is unchanged and that the provisioned user sees nothing.
   - An identity cannot move between principals.
5. **Operator recovery** (lost portal access, wrong link, or IdP migration). These are trusted operations with database access, like `init`:
   - `team-memory principal list` / `principal identities --principal <id>` to inspect.
   - `team-memory token create --principal <owner-id>` to regain access with a new PAT.
   - `team-memory principal link-identity --principal <id> --issuer <iss> --subject <sub>` to attach an identity. The operator takes `sub` from the IdP admin console, never an email. It refuses identities linked elsewhere, and is audited.
   - Unlinking or moving an identity is not exposed in v1. Recovery is to disable the wrongly linked principal; stage 04 decides whether an audited `unlink` command is needed.
   - Upstream issuer change (IdP migration): add the new provider, have each user link the new identity through the portal (or the operator runs `link-identity` per user), then remove the old provider.

## Kubernetes configuration contract

SSO is enabled by `TEAM_MEMORY_SSO_CONFIG=/etc/team-memory/sso/sso.json`, a JSON file from a ConfigMap. The file contains **no secrets**; every secret is a file path into a mounted Secret. Unknown fields are rejected, so an inline `client_secret` fails readiness.

```json
{
  "issuer": "https://memory.example.com",
  "signing_keys_dir": "/var/run/secrets/team-memory/signing",
  "active_signing_key": "2026-10",
  "access_token_ttl": "15m",
  "refresh_token_ttl": "720h",
  "client_registration": {
    "client_id_metadata_documents": true,
    "dynamic": true,
    "cimd_allowed_hosts": [],
    "allow_remote_redirect_uris": false
  },
  "providers": [
    {
      "id": "corp",
      "display_name": "Example Corp SSO",
      "issuer": "https://login.example.com/realms/corp",
      "client_id": "team-memory",
      "client_secret_file": "/var/run/secrets/team-memory/idp-corp/client-secret",
      "scopes": ["openid", "email", "profile"],
      "allowed_email_domains": ["example.com"],
      "allowed_groups": [],
      "groups_claim": "groups",
      "auto_provision": true
    }
  ]
}
```

| Value | Where | Notes |
| --- | --- | --- |
| `issuer` | ConfigMap, or `TEAM_MEMORY_PUBLIC_URL` | The public origin, with no path. Becomes the token `iss`. `<issuer>/mcp` is the resource and token audience. Must be https; `allow_insecure_http` is loopback-only and for development. |
| Signing keys | Secret `team-memory-signing`, keys `<kid>.pem`, mounted at `signing_keys_dir` | P-256 EC (ES256, preferred) or RSA ≥ 2048 (RS256), PKCS#8/SEC1/PKCS#1 PEM. The file name is the `kid`. Generate with `openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256`. |
| `active_signing_key` | ConfigMap | Required when more than one key is present. |
| Per provider `issuer`, `client_id` | ConfigMap | `issuer` must exactly match the IdP's `iss`, because go-oidc checks it. |
| Per provider client secret | Secret `team-memory-idp-<id>`, key `client-secret`, path in `client_secret_file` | Read on every use. |
| Upstream redirect URI to register at the IdP | n/a | `<issuer>/oauth/callback/<provider id>`. Confidential client, authorization-code flow, PKCE S256. |
| `allowed_email_domains` / `allowed_groups` | ConfigMap | A domain rule requires `email_verified` true and an exact domain match. A group rule requires one listed group in `groups_claim`. Empty means any user of that IdP may sign in (with no roles). |
| `client_registration` | ConfigMap | CIMD and/or DCR. `cimd_allowed_hosts` optionally limits metadata-document hosts. `allow_remote_redirect_uris` defaults to false: only loopback redirect URIs are accepted, which is what Claude Code's and Codex's CIMD documents list. Enable it only for browser-based MCP clients. Any app that persuades a user to consent can receive a code at its own https redirect. |
| `auto_provision` | ConfigMap | Default true: a principal is created, with no roles, on first login. If false, only identities linked in advance (by the portal or `link-identity`) may sign in. |

**Multiple issuers per organization:** list several providers. Each gets its own callback path, Secret and allow rules, and the consent page offers one button per provider. Identities are keyed by issuer, so the same person from two IdPs is two identities, joined only by explicit linking. The prototype serves one organization per deployment (`init` allows one); a per-provider `org` field is deferred until multi-org deployments exist.

**Rotation without downtime:**

- *Client secret:* create the new secret at the IdP while the old one stays valid (Keycloak, Entra and Okta support overlapping secrets), then update the Kubernetes Secret. The kubelet refreshes the mounted file, typically within a minute, and Team Memory reads it on the next login. Revoke the old secret at the IdP afterwards. Secrets mounted with `subPath` do not refresh, so do not use `subPath`.
- *Signing key:*
  1. Add `<new>.pem` to the Secret; every key is published in the JWKS and accepted.
  2. Restart or roll the pods: keys are read at startup, so hot reload is a stage 04 item.
  3. Set `active_signing_key` to the new key and roll again.
  4. After `access_token_ttl` has passed, remove the old file and roll.
  
  Refresh tokens are opaque and survive key rotation. Authorization codes in flight (≤ 10 min) are encrypted with a key derived from the active signing key and fail across the switch; clients simply sign in again.

**Readiness:** with `TEAM_MEMORY_SSO_CONFIG` set, `/readyz` returns 503 with the reason, and **every `/mcp`, `/oauth/*` and `/.well-known/*` request returns 503**, PATs included, until all of these hold:

- the config parses with no unknown fields;
- every signing key loads;
- every client-secret file is present and non-empty, rechecked continuously;
- every provider's discovery has succeeded and advertises S256.

Messages name the provider, field or file, never a secret value. Startup does not crash-loop on a temporarily unreachable IdP; discovery retries with backoff up to 1 min. There is no fallback to PAT-only or unauthenticated operation once SSO is configured. Tests: `TestSSOMisconfigurationFailsClosed` (missing secret, empty secret, missing keys, inline secret, unreachable IdP, http issuer) and `TestSSOSecretRemovedAtRuntime`.

## Roles: database membership vs IdP groups

**Recommendation: database-managed membership (`scope_members`) is authoritative.** IdP groups may only gate sign-in in v1.

- For the database: it works identically for PAT-only solo installs and every IdP. It supports the scope tree, cascading grants and maintainers per scope, which IdP groups do not model. Revocation is immediate on the next request. Changes are auditable in Team Memory.
- For IdP groups: central administration, and joiners/leavers handled at the IdP. But group claims differ per IdP (names vs object IDs, size limits, Entra overage), they arrive only at login (stale for the life of a refresh token), and mapping groups onto a tree of scopes needs a per-org mapping table.
- Optional later step (stage 04 or after): a per-org `group_role_mappings (issuer, group, scope, role, cascade)` synchronized at login, writing ordinary `scope_members` rows marked `source='idp'`, so the database stays the single place authorization reads.

## What remains unverified or deferred

- **OAuth state is in memory** (`internal/auth/oauth/store.go`): DCR clients, pending authorizations, codes and refresh tokens. A restart loses refresh tokens and DCR registrations, and multiple replicas would not share them. Access tokens and CIMD clients survive restarts (**run**: Claude Code stayed connected after a restart). Stage 04 must move this to PostgreSQL before more than one replica or any real use.
- **RFC 9207 `iss`** in authorization responses is not emitted.
- **Consent is not remembered**; every login shows it. **CSRF** protection on the consent form is a double-submit cookie plus an `Origin` check, and the callback is bound to the browser by cookie.
- **Signing-key hot reload** and a key-generation CLI.
- **Clients:** pre-registered-client flows (`--client-id`, `--oauth-client-id`) were not run. Windows and Linux clients and other client versions were not run. Full verification of both clients is stage 07.
- **Real enterprise IdPs** (Entra, Okta, Google) were not run. Their DCR, audience and claim behavior above is **read**, not tested. Group-claim gating is tested only against the fake IdP (`TestOAuthGroupGate`), not a real one.
- **Concurrent refresh.** Rotation plus reuse detection revokes the whole token family if a client sends the same refresh token twice. Claude Code refreshed four times in quick succession, but sequentially; no concurrent reuse was observed. Stage 04 should consider a short grace window (for example 10 s) in which the just-rotated token returns the same successor, so a client race does not force a new sign-in.
- **SAML** via a broker (Keycloak or Dex in front) is documented, not built or tested.
- **Rate limiting** of `/oauth/register`, `/oauth/authorize` and the CIMD fetcher is not done (the counts are bounded at 10,000 entries).
- `/readyz` returns the failure reason to any caller. It names files and providers, not secrets. Stage 06 may restrict it.
- Upstream `prompt=login` for portal linking is specified, not built (stage 04/05).

## Consequences for stage 04

- Finalize `internal/auth/oauth` as production code. It is enabled only by `TEAM_MEMORY_SSO_CONFIG`; there is no separate build flag. Persist the store in PostgreSQL, add RFC 9207 `iss`, rate limits, key hot reload, and audit events for sign-in success/failure and denials.
- Keep `auth.Chain` semantics: PATs by prefix, everything else must be a Team Memory JWT. Never add upstream-token acceptance.
- Use `Principal.AuthMethod` (`pat` | `oauth`) together with the portal session to keep approvals browser-only.
- The adversarial tests listed in stage 04 for tokens map onto `internal/auth/oauth/tokens_test.go` and `integration/oauth_test.go`; extend them rather than duplicating them.
