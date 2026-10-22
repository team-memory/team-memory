# Connecting Claude Code

Verified against Claude Code 2.1.292 (stage 01 PAT smoke test, stage 03 OAuth). Claude Code 2.1.304 is installed now and has not been re-run; see `docs/verification/cross-client.md` for what is still unverified.

## With a personal access token (solo)

```bash
claude mcp add --transport http --scope user team-memory https://memory.example.com/mcp --header "Authorization: Bearer $TEAM_MEMORY_TOKEN"
```

The `--header` form stores the token in Claude Code's config. Prefer a short-lived token (`team-memory token create --ttl 720h`) and revoke it with `token revoke` if the machine is lost. Never commit the config.

## With SSO (OAuth)

```bash
claude mcp add --transport http --scope user team-memory https://memory.example.com/mcp
```

Then run `/mcp` inside Claude Code (or `claude mcp login team-memory`) and sign in through the browser. Claude Code discovers the authorization server from the `/mcp` 401 response, registers via a client-ID metadata document, and signs in through your IdP. Verified in stage 03 with Keycloak. A first sign-in creates a principal with no roles; an operator or maintainer must grant a role before tools succeed (`permission_denied` until then).

## Check it works

Ask Claude Code to call `memory_search` for a known topic. Results start with an `UNTRUSTED DATA` notice and carry `trust`, `origin_scope`, `inherited` and `revision`.

## Troubleshooting

- **401 / not connected:** token expired or revoked, or SSO not ready. Check `https://memory.example.com/readyz`; it reports not ready when SSO secrets are missing.
- **`permission_denied`:** the principal has no role on the scope.
- **Self-signed certificate:** set `NODE_EXTRA_CA_CERTS` to your CA file.
- **`memory_approve` does not approve:** by design it only requests review; a maintainer approves in the portal.

Add the policy snippet in `docs/agent-policy/CLAUDE.md` to your repository's `CLAUDE.md`.
