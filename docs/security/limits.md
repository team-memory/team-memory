# Input limits and rate limits

All limits are enforced by the server. Field limits are enforced in Go and again by database `CHECK` constraints.

## Request size and timeouts (serve)

| Limit | Value | Response |
| --- | --- | --- |
| MCP request body | 256 KiB (`mcp.DefaultMaxBodyBytes`) | 413 before the body is parsed |
| Request headers | 64 KiB | 431 |
| Read header / read / write / idle timeouts | 10 s / 30 s / 60 s / 120 s | connection closed |
| OAuth registration body | 16 KiB | `invalid_client_metadata` |
| Client metadata document fetch | 64 KiB, 5 s, public addresses only | client refused |
| Logged header values | truncated (method 64, protocol 32, user agent 200 characters) | n/a |

## Memory fields

| Field | Limit |
| --- | --- |
| title | 1–200 characters |
| content | 1–20,000 characters |
| rationale | 10,000 characters |
| source_uri | 2,048 characters, http(s) only, no embedded credentials |
| change_note, supersede reason, archive reason | 1,000 characters |
| review note, promotion rationale | 2,000 characters |
| override links per revision | 10 |
| search query | 500 characters |
| results per call | default 10, maximum 50 |

## Secret scanning

Create, update, supersede, approval, promotion submission and promotion finalization reject text that matches a credential rule (`internal/secrets`). The rules cover private key blocks, AWS access key IDs and secret assignments, GitHub, GitLab, Slack, Google, Stripe, OpenAI, Anthropic and npm tokens, Team Memory tokens, JWTs, URLs with embedded passwords, and high-entropy values assigned to names like `password`, `secret`, `api_key` or `client_secret`. The error names the field and rule type, never the matched text. The tool error code is `secret_detected`. Detection is best-effort: it reduces accidents but cannot catch every secret format, so treat a stored secret as exposed and rotate it.

## Rate limits

Limits are per process. With N replicas, the effective limit is up to N times higher.

| What | Key | Default | Setting |
| --- | --- | --- | --- |
| MCP requests | principal | 20/s, burst 60 | `TEAM_MEMORY_RATE_LIMIT`, `TEAM_MEMORY_RATE_BURST` (negative rate disables) |
| `/oauth/register`, `/oauth/authorize`, `/oauth/login`, `/oauth/callback/*`, `/oauth/token` | client address | 60/min | `oauth.Options.RatePerMinute` |
| Audit rows for rejected credentials | client address | 1/s, burst 20 | fixed; excess failures are only logged |
| DCR clients, pending authorizations, pending upstream logins | global | 10,000 each | fixed |

The client address is the TCP peer address. Behind a reverse proxy, every request shares the proxy's address. Stage 06 must either configure trusted proxy headers or rely on the ingress for per-client limits; until then, OAuth limits are effectively global behind a proxy.

Query embedding (Ollama) runs per search request, so the MCP rate limit is also the embedding rate limit.
