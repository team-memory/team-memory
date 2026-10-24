# Decision: client attribution on revisions and audit events

Status: accepted (2026-10-24)

## Decision

Capture a bounded, transport-independent attribution snapshot from the authenticated request. The MCP adapter uses the SDK request ClientInfo and ProtocolVersion methods. OAuth carries the verified access token's client_id and a best-effort registered application-name snapshot; PAT authentication carries the token's database ID and label, never its secret or hash. Registry lookup failure leaves the application name empty and does not reject an otherwise valid token.

Write public attribution on immutable memory revisions and lifecycle event snapshots, and full attribution on restricted audit events. These writes share the mutation transaction. Revision 1 supplies creation provenance; the current revision supplies last-edit provenance. Promotion records the promoting request on the destination and keeps the existing source link. Existing rows retain empty attribution rather than guessing a client from the author.

Public attribution contains channel, authentication method, MCP client name/version and source, OAuth application name, tool, protocol version and server version when available. OAuth/PAT identifiers, PAT labels and request/trace IDs remain in the maintainer/operator audit log. Names are descriptive and escaped in HTML; even a token-associated OAuth client ID does not attest the software actually running. None of these fields changes authorization or approval.

The endpoint remains stateless. Modern clients can supply client info per request; legacy initialization-only metadata may not survive to tools/call. Show “Not recorded” rather than caching attribution by principal/token (which could conflate concurrent clients). Local MCP connection aliases are not transmitted and are not inferred. No arbitrary headers, IP addresses, transcripts, model names or filesystem paths are collected.

## UI and operations

Memory provenance and revision history expose client summaries and expandable metadata. Scope maintainers can open Audit log from the scope page. Exact filters run in SQL before the result limit: actor UUID, client name/application name/OAuth client ID, action, outcome, target ID, and UTC date bounds. The page shows the newest 50 matching events; narrow dates for older records. Scope-less denials remain operator-only. The existing RLS and append-only triggers continue to apply.

Migration 00008 adds a JSON object on memory_revisions with a size constraint and an empty default; sqlc output is regenerated. Git mirror documents intentionally continue their existing allowlist and omit client attribution; a mirror rebuild cannot recover attribution. The authoritative provenance is in PostgreSQL and database backups.

## Validation

Protocol integration tests cover modern and legacy MCP calls. Adversarial tests cover separate clients across revisions, public redaction, audit filtering and scope isolation, and HTML escaping of client names. Tests also exercise legacy empty attribution and bounded Unicode metadata.
