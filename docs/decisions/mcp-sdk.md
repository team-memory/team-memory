# Decision: MCP SDK

Status: accepted (stage 01, 2026-10-20)

## Decision

Use the official Go SDK, `github.com/modelcontextprotocol/go-sdk` v1.8.0, with `StreamableHTTPOptions{Stateless: true, JSONResponse: true}`, behind the SDK's `auth.RequireBearerToken` middleware.

## Context checked against current sources

- The current MCP specification is revision **2026-07-28** ([changelog](https://modelcontextprotocol.io/specification/2026-07-28/changelog)). It removes protocol-level sessions and the `initialize` handshake. Each request carries its protocol version and client capabilities in `_meta`, and servers must implement `server/discover`. It also adds `subscriptions/listen`, requires `Mcp-Method`/`Mcp-Name` headers, and deprecates Dynamic Client Registration in favor of Client ID Metadata Documents.
- go-sdk v1.7.0 added full support for 2026-07-28, which over HTTP is accepted only in stateless mode. v1.8.0 (2026-09-14) keeps 2026-07-28 as the newest version and preserves backward compatibility with 2025-11-25 and earlier ([releases](https://github.com/modelcontextprotocol/go-sdk/releases)).
- The SDK ships an `auth` package (bearer-token middleware, protected-resource metadata handler, OAuth client helpers). Stage 03 builds on it.

## Observed client behavior (installed versions)

From server logs during the early smoke test (`docs/verification/early-clients.md`):

| Client | Protocol version used | Handshake |
| --- | --- | --- |
| Claude Code 2.1.292 | 2026-07-28 | `server/discover`, then `tools/list`, `resources/list`, `subscriptions/listen` |
| Codex CLI 0.163.0 | 2025-06-18 | legacy `initialize` |

Both clients are in use today, so the server must speak both protocol generations. Stateless mode does this.

## Consequences

- The stage 01 acceptance wording "initialize, list tools, ..." is satisfied as: legacy clients `initialize`, and 2026-07-28 clients call `server/discover`. The protocol test (`integration/mcp_test.go`) covers both, using the SDK client pinned to each version.
- No server-to-client requests (sampling, elicitation) are available in stateless mode under the legacy protocol. Team Memory does not need them, and the 2026-07-28 MRTR pattern replaces them.
- Tool errors are returned as `isError` results with a structured `{"error": {"code", "message", ...}}` body, so agents can react to `conflict`, `not_found`, `permission_denied`, `inherited_read_only` and `not_implemented`.

## Alternatives considered

- **mark3labs/mcp-go**: widely used community SDK. Not chosen because the official SDK tracked the 2026-07-28 revision first and has an auth package maintained with the spec. Its current spec coverage was not re-verified for this decision.
- **Hand-rolled JSON-RPC**: rejected. The protocol is changing quickly (sessions removed, new headers, MRTR), and tracking it by hand is a maintenance and security risk.
