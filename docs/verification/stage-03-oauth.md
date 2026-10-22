# Stage 03 OAuth verification with real clients

Date: 2026-10-21. Machine: a macOS workstation (darwin/arm64).

Setup:
- **Server:** `team-memory serve` from this branch, with `TEAM_MEMORY_SSO_CONFIG` pointing at the `deploy/dev-idp` configuration.
- **Database:** a throwaway database (`teammemory_sso03`) in the local compose PostgreSQL, initialized with `team-memory init --org devorg --project sso`.
- **Issuer:** `http://localhost:8080`, with `allow_insecure_http` for loopback development.
- **Upstreams:** the disposable stack in `deploy/dev-idp/` (Keycloak 26.8.1, Dex v2.46.0). Users and secrets came from `setup.sh` and are not recorded here.

This verifies the selected architecture end to end. It is not the stage 07 release verification, and the identities are test users, not pilot evidence.

## Client versions

| Client | Version |
| --- | --- |
| Claude Code | 2.1.292 |
| Codex CLI | 0.163.0 |

## How the logins were driven

Both clients ran their own OAuth flow with `mcp login <name> --no-browser` inside a pseudo-terminal:
- `claude mcp login tm-sso03 --no-browser`
- `codex mcp login tm_sso03 --no-browser -c 'mcp_servers.tm_sso03.url="http://localhost:8080/mcp"'`

A small script played the user's browser. It opened the authorization URL the client printed, read the Team Memory consent page, pressed "Continue with <provider>", and submitted the IdP's login form with the generated test user. It stopped at the client's loopback callback and pasted that URL back into the client, which then did its own code exchange.

What the script did *not* do: discovery, registration, PKCE, token exchange, refresh or the MCP calls. Those were all the clients' own.

Configuration changes:
- **Claude Code:** a local-scope server entry for a scratch project directory, removed afterwards with `claude mcp remove`.
- **Codex:** only `-c` overrides; `~/.codex/config.toml` was compared before and after and was unchanged. Its stored OAuth credentials were removed with `codex mcp logout`.

## Results

| Step | Claude Code 2.1.292 (upstream: Keycloak, user alice@example.com) | Codex 0.163.0 (upstream: Dex, no DCR, user bob@example.com) |
| --- | --- | --- |
| Unauthenticated request | `POST /mcp` (2026-07-28 `server/discover`) → 401 with `resource_metadata` and `scope="memory"` | `GET /mcp` (2024-11-05) → 401 |
| Discovery | `GET /.well-known/oauth-protected-resource/mcp`, then `GET /.well-known/oauth-authorization-server` | Same two URLs |
| Registration | CIMD: `client_id=https://claude.ai/oauth/claude-code-client-metadata`, which Team Memory fetched and validated; consent page showed "Claude Code" | CIMD: `client_id=https://chatgpt.com/oauth/codex/<callback-id>/client.json`; consent page showed "Codex" |
| Authorization request | `scope=memory offline_access`, `resource=http://localhost:8080/mcp`, S256, `redirect_uri=http://localhost:<port>/callback`, `prompt=consent` | `scope=memory offline_access`, `resource=http://localhost:8080/mcp`, S256, `redirect_uri=http://127.0.0.1:<port>/callback/<callback-id>` |
| Upstream login | Keycloak; principal "Alice Example" auto-provisioned, audit event `auth.principal_provisioned` | Dex; principal "bob" auto-provisioned |
| Token | `POST /oauth/token` 200 | `POST /oauth/token` 200 |
| Authenticated tools (after the operator granted `writer` to Alice and `reader` to Bob on `devorg/sso`) | `memory_create` → id `93b7fd0b-…`, trust `cited`, `created_by_name` "Alice Example"; `memory_search` found it | `memory_search` + `memory_get` returned the same memory, created by "Alice Example"; `memory_create` → `permission_denied` (reader) |
| MCP protocol after login | 2026-07-28 (`server/discover`, `tools/list`, `resources/list`, `subscriptions/listen`, `tools/call`) | `initialize` (2025-06-18), `notifications/initialized`, `tools/list`, `tools/call` |
| Restart of Team Memory (in-memory OAuth state lost) | Still connected: its access token is a JWT validated statelessly with the unchanged signing key | Logged in again |
| Refresh (server restarted with `access_token_ttl: 1m`, sign-in repeated, then a 75 s wait) | `POST /oauth/token` ×4 (sequential, within 12 s), then `tools/call` 200 | `POST /oauth/token` ×1, then `tools/call` 200 |

Overall: **pass for both clients.** The acceptance gate needs at least one client; both passed.

**Re-run on the final build.** After the review fixes (loopback-only redirect URIs by default, auth-request copies, 503 on `/.well-known/` while not ready), both clients signed in again against a rebuilt server:
- Claude Code with Keycloak, as Alice.
- Codex with Dex, as Bob.

Each ran `memory_search` successfully and found memory `93b7fd0b-…`. Both CIMD documents list only loopback redirect URIs (`http://localhost/callback`, `http://127.0.0.1/callback`), so the new default does not affect them.

Two failures during the run came from the test driver, not from Team Memory or the clients:
- The first attempt grabbed the text after the printed URL and sent `resource=…/mcpWaiting`. Team Memory correctly answered `invalid_target`.
- The driver's cookie jar did not send Keycloak's `Secure` cookies to `http://localhost`, which browsers do. Keycloak returned 400.

Both were fixed in the driver.

## Architecture B (resource server only) with the same clients

A stub resource server served protected-resource metadata naming the upstream IdP directly as its authorization server.

| Upstream named in metadata | Claude Code 2.1.292 | Codex 0.163.0 |
| --- | --- | --- |
| Dex v2.46.0 (no `registration_endpoint`, no CIMD) | "Incompatible auth server: does not support dynamic client registration" | "Dynamic client registration not supported" |
| Keycloak 26.8.1, default realm policies | DCR rejected, HTTP 403 "Policy 'Allowed Client Scopes' rejected request" | Same 403 |

Neither client can use B without per-client pre-registration at each IdP. This was not tried with pre-registration (`--client-id` / `--oauth-client-id`).

## Upstream metadata observed

| IdP | `registration_endpoint` | `client_id_metadata_document_supported` | `code_challenge_methods_supported` | `authorization_response_iss_parameter_supported` | RFC 8414 well-known |
| --- | --- | --- | --- | --- | --- |
| Keycloak 26.8.1 | present | absent | plain, S256 | true | yes |
| Dex v2.46.0 | absent | absent | S256, plain | absent | no (OIDC discovery only) |

## Client quirks

- **Codex** probes with an unauthenticated `GET /mcp` at protocol 2024-11-05 before each session, even with a stored token. It then fetches both metadata documents again before its `initialize` POST. The 401 is expected.
- **Codex** registers `http://127.0.0.1/callback/<id>` without a port in its CIMD document and listens on a random port. The AS must match loopback redirects without the port (RFC 8252 §7.3); zitadel's native-client matching does this.
- **Claude Code** sends `prompt=consent`. Team Memory always shows consent anyway.
- **Claude Code** refreshed four times in about 12 s within one `-p` run, sequentially. With strict rotation and reuse detection, a *concurrent* duplicate would revoke the family. See the stage 04 note in the decision record.
- **Claude Code** printed an SDK notice that the stored credential has no RFC 9207 issuer stamp. Team Memory does not emit `iss` in authorization responses yet.
- Both clients requested `offline_access` because the AS metadata lists it, and neither sent `openid`.
- Codex logged unrelated errors while refreshing its model list from a model proxy. They did not affect MCP.

## Cleanup

- Claude Code: `claude mcp remove tm-sso03`, `b-dex` and `b-kc` (local scope, scratch project).
- Codex: `codex mcp logout tm_sso03` with the same `-c` override.
- The `teammemory_sso03` database and the dev IdP containers are disposable. Remove them with `docker compose -f deploy/dev-idp/compose.yaml down` and `DROP DATABASE teammemory_sso03`.
