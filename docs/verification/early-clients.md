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
| Codex 0.163.0 | 2025-06-18 | Methods not visible: before the 2026-10-21 logging fix, these requests were logged with an empty method. The 2025-06-18 version came from the header. The legacy handshake was inferred from that version, not seen in the log. The 2026-10-21 run logged `initialize`, `notifications/initialized`, `tools/list`, `tools/call`. |

The server must therefore keep supporting both protocol generations (see `docs/decisions/mcp-sdk.md`).

## Re-verification (2026-10-21)

Same machine, client versions and server setup as above, using a separate 4-hour PAT (`smoke-2`) that was revoked afterwards. This run covered the steps of the acceptance path not run on 2026-10-20. Create and keyword search rely on the earlier run.

The Claude Code run allowed these tools:

```bash
TM_TOKEN=... claude -p --mcp-config claude-mcp.json --strict-mcp-config \
  --allowedTools "mcp__team-memory__memory_get,mcp__team-memory__memory_update,mcp__team-memory__memory_history,ReadMcpResourceTool,ListMcpResourcesTool" \
  "<prompt>" < /dev/null
```

Codex used the same command as above.

| Direction | Steps | Observed | Result |
| --- | --- | --- | --- |
| Claude Code | `memory_get`, then `memory_update` on the Codex-created `70783a3e-…` with `expected_revision` 1 | Revision 2, trust stays `unverified` | Pass |
| Claude Code | `memory_update` again with stale `expected_revision` 1 | `conflict`, `current_revision` 2 | Pass |
| Claude Code | `memory_history`; `resources/read` of `memory://scopes/acme/smoke/recent` | Revisions 1 and 2, events `created` and `revised`; the resource lists both memories, `inherited: false` | Pass |
| Codex | `memory_history` and the same resource | Both revisions with the Claude Code change note and appended sentence; the resource shows `70783a3e-…` at revision 2 | Pass |
| Codex | `memory_recent` | Both ids returned; the server log shows `initialize`, `notifications/initialized`, `tools/list`, `tools/call` | Pass |

**Missing `memory_recent` call explained.** The earlier version-capture run passed `--allowedTools` with only create, search and get. In `-p` mode, Claude Code denies any other tool before it sends a request, so the server never received a `tools/call`. Reproduced: with the 2026-10-20 `--allowedTools` list above, the command reports `permission_denials: [mcp__team-memory__memory_recent]`, and the server sees only discovery and listing. With the tool allowed, the log shows `tools/call`. It was not a server issue.

**Logging fix.** The request log took the method only from the `Mcp-Method` header. Only 2026-07-28 clients send that header, so every Codex (2025-06-18) request was logged with an empty method. Now, when the header is missing, `logRequests` falls back to the JSON-RPC `method` member:
- It records the first 64 KiB of the body as the handler reads it, after authentication, and does not read ahead. Rejected requests still get an immediate 401, with no `100 Continue`, and log an empty method.
- It scans top-level keys in order, so large requests are still identified when `method` precedes the cut.
- It logs no body content.

The code is in `cmd/team-memory/main.go`, tested in `main_test.go`. Confirmed against a running server: an authenticated header-less `initialize` logged `initialize`, and an unauthenticated `Expect: 100-continue` POST got 401 directly.

## Observations and gaps

- Codex logged unrelated errors refreshing its model list from the author's configured model proxy. They did not affect MCP calls.
- Real clients have now exercised `memory_create`, `memory_search`, `memory_get`, `memory_update` (including the stale-revision conflict), `memory_history`, `memory_recent` and one resource. `memory_supersede` and `memory_approve` are covered only by the SDK-client protocol test (`integration/mcp_test.go`).
- Not tested: OAuth (stage 03), Windows/Linux clients, and other machines.
