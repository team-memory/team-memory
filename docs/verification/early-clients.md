# Early cross-client smoke test (stage 01)

Date: 2026-10-20. Machine: the author's macOS workstation (darwin/arm64). Server: `team-memory serve` from this repository at the stage 01 commit, against the local compose PostgreSQL 17 + pgvector 0.8.1, listening on `127.0.0.1:18080/mcp`. Authentication: a personal access token issued by `team-memory init`.

This is a basic PAT smoke test with synthetic data in one scope (`acme/smoke`). It is not the stage 07 release verification, does not exercise OAuth, and is not pilot evidence.

## Client versions

| Client | Version |
| --- | --- |
| Claude Code | 2.1.292 |
| Codex CLI | 0.163.0 |

Neither client's persistent configuration was changed. Each run passed the server on the command line.

Claude Code (`--mcp-config` file with `"headers": {"Authorization": "Bearer ${TM_TOKEN}"}`):

```bash
TM_TOKEN=... claude -p --mcp-config claude-mcp.json --strict-mcp-config \
  --allowedTools "mcp__team-memory__memory_create,mcp__team-memory__memory_search,mcp__team-memory__memory_get" \
  "<prompt>"
```

Codex (config overrides, token read from the environment):

```bash
TM_TOKEN=... codex exec --skip-git-repo-check -s read-only \
  -c 'mcp_servers.team_memory.url="http://127.0.0.1:18080/mcp"' \
  -c 'mcp_servers.team_memory.bearer_token_env_var="TM_TOKEN"' "<prompt>"
```

## Results

| Direction | Steps | Observed | Result |
| --- | --- | --- | --- |
| Claude Code → Codex | Claude Code `memory_create` (decision, with `source_uri`); Codex `memory_search` + `memory_get` | Created `bd46370b-…` with trust `cited`, revision 1. Codex returned the same id, title, trust, revision, `created_by_name`, and `origin_scope` `acme/smoke`. | Pass |
| Codex → Claude Code | Codex `memory_create` (convention, no citation); Claude Code `memory_search` + `memory_get` | Created `70783a3e-…` with trust `unverified`, revision 1. Claude Code returned the same fields plus `inherited: false`, and noted the memory is a proposal, not approved policy. | Pass |
| Unauthenticated | `POST /mcp` without a token | HTTP 401 | Pass |

The database afterwards held exactly these two memories, each with one `created` lifecycle event.

## Protocol versions seen in server logs

| Client | `Mcp-Protocol-Version` | Calls observed |
| --- | --- | --- |
| Claude Code 2.1.292 | 2026-07-28 | `server/discover`, `tools/list`, `resources/list`, `subscriptions/listen` |
| Codex 0.163.0 | 2025-06-18 | legacy `initialize`, then tool calls |

The server must therefore keep supporting both protocol generations (see `docs/decisions/mcp-sdk.md`).

## Observations and gaps

- In the version-capture run, the Claude Code log line set showed discovery and listing but no `tools/call` entry for the `memory_recent` request. Tool calls by Claude Code were confirmed in the two direction tests above, through the returned ids, which match the database. This was not investigated further.
- Codex logged unrelated errors refreshing its model list from the author's configured model proxy. They did not affect MCP calls.
- Only `memory_create`, `memory_search`, `memory_get` and `memory_recent` were exercised by real clients. The remaining tools are covered by the SDK-client protocol test (`integration/mcp_test.go`), not by these two clients.
- Not tested: OAuth (stage 03), resources from the real clients, Windows/Linux clients, and other machines.
