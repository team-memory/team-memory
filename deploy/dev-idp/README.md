# Local SSO development stack

This stack is disposable and for development and tests only. It runs two identity providers on loopback, so you can exercise Team Memory's OAuth facade with real MCP clients:

| IdP | URL | Why it is here |
| --- | --- | --- |
| Keycloak 26.8.1 | `http://localhost:8180/realms/team-memory-dev` | A full-featured IdP. Team Memory uses it only as a confidential OIDC client. |
| Dex v2.46.0 | `http://localhost:5556/dex` | Has **no dynamic client registration and no client ID metadata documents**, like most enterprise IdPs. It proves the facade does not need either. |

Nothing in this directory is a secret. `setup.sh` generates random client secrets, user passwords and a token-signing key into `.local/`, which is git-ignored and readable only by you. It also writes `.local/sso.json`, a Team Memory SSO configuration for both providers that allows only `example.com` email addresses.

## Run

```bash
./deploy/dev-idp/setup.sh
```

```bash
docker compose -f deploy/dev-idp/compose.yaml up -d --wait
```

Team Memory must run at `http://localhost:8080`. The IdP clients are registered with `http://localhost:8080/oauth/callback/{keycloak,dex}`. Use a database of its own, because `init` refuses to run twice:

```bash
TEAM_MEMORY_SSO_CONFIG=$PWD/deploy/dev-idp/.local/sso.json TEAM_MEMORY_PUBLIC_URL=http://localhost:8080 ./bin/team-memory serve
```

Then add `http://localhost:8080/mcp` to a client and sign in. With Claude Code, run `claude mcp add --transport http tm-dev http://localhost:8080/mcp`, then `claude mcp login tm-dev`. With Codex, run `codex mcp add tm_dev --url http://localhost:8080/mcp`, then `codex mcp login tm_dev`.

The test users are in `.local/env`:
- `alice@example.com` (Keycloak): allowed.
- `eve@evil.test` (Keycloak): refused by the domain rule.
- `bob@example.com` (Dex): allowed.

A first sign-in creates a principal with no roles. Grant one with `team-memory member add`.

## Stop and reset

```bash
docker compose -f deploy/dev-idp/compose.yaml down
```

Delete `.local/` to generate new credentials. After `down`, the IdPs start fresh: Keycloak re-imports the realm, and Dex uses memory storage.
