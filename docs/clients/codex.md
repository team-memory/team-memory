# Connecting Codex

Verified against Codex CLI 0.163.0 (stage 01 PAT smoke test, stage 03 OAuth). That is also the version installed now.

## With a personal access token (solo)

```bash
codex mcp add team_memory --url https://memory.example.com/mcp --bearer-token-env-var TEAM_MEMORY_TOKEN
```

This writes to `~/.codex/config.toml`. The equivalent entry:

```toml
[mcp_servers.team_memory]
url = "https://memory.example.com/mcp"
bearer_token_env_var = "TEAM_MEMORY_TOKEN"
```

The token stays in the environment variable, not in the file. Export it in the shell that launches Codex.

## With SSO (OAuth)

```bash
codex mcp add team_memory --url https://memory.example.com/mcp
codex mcp login team_memory
```

Verified in stage 03 with Dex as the upstream (Dex has no dynamic client registration; Codex's client-ID metadata document was accepted). Grant the new principal a role before expecting tools to work.

## Check it works

Ask Codex to search Team Memory. A reader can search and get; `memory_create` returns `permission_denied` for readers.

## Troubleshooting

- **Login loops or fails:** confirm `/.well-known/oauth-protected-resource/mcp` on the server returns JSON and `/readyz` is ok.
- **Tool not visible:** run `codex mcp list`; the server name must match `[mcp_servers.<name>]`.
- **Self-signed certificate:** add your CA to the system trust store.

Add the policy snippet in `docs/agent-policy/AGENTS.md` to your repository's `AGENTS.md`.
