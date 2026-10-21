# Landscape review (stage 01)

Reviewed 2026-10-20. This is a focused check of existing shared-memory MCP projects and repository-based workflows before implementation, not a market survey. Claims about other projects come from their own READMEs or docs pages as fetched on that date. They were not independently tested, and project activity may have changed since. Nothing here establishes that Team Memory is better than any alternative or that there is demand for it.

## Repository-based workflows

`CLAUDE.md`, `AGENTS.md`, ADRs and wikis are already shared through git. They give versioning, review through pull requests, and access control through repository permissions at no extra cost. Their limits, relative to the proposal, are:

- Access control is per repository, not per scope or row, so organization-wide guidance is copied into each repository or kept somewhere else.
- No retrieval: agents load whole files into context or rely on client-specific discovery.
- Agents that write to these files either need commit access (no human gate) or go through pull requests, which is a heavier workflow than a review queue.

Overlap: PR review is a strong, familiar approval mechanism. The proposal defers PR-based approval and picks a mirror format that keeps it possible later. For a single repository and a single team, plain repository files may be enough. That is a real alternative to keep in mind during the pilot.

## Shared-memory MCP servers

| Project | Storage | Team model | Approval / trust | Auth | License |
| --- | --- | --- | --- | --- | --- |
| [mcp-memory-service](https://github.com/doobidoo/mcp-memory-service) | SQLite, Cloudflare, hybrid, Milvus | Shared across agents; `X-Agent-ID` scoping; SQLite described as single-user | Quality scoring, confidence scores, heuristic contradiction detection; no human review described | OAuth 2.0 + DCR for remote MCP; quick start shows anonymous access enabled | Apache-2.0 |
| [global-agent-memory](https://github.com/ozankasikci/global-agent-memory) | Markdown vault (canonical) + SQLite FTS5/vectors | Single owner, many agents | Agents create candidates; owner approves, edits, rejects in a local dashboard | Localhost bearer token; one-time dashboard URLs | MIT |
| [ai-memory](https://github.com/williamlodge/ai-memory) | SQLite + FTS5 on a VPS | Many agents, described as single-user | None described | Per-agent hashed keys; every key has full access | Not stated |
| [Basic Memory](https://docs.basicmemory.com) | Markdown knowledge graph | Teams with roles and invites (cloud) | Comments and suggestions (cloud); role details not documented on the page | Not described on the page | Open-source local version plus a commercial cloud |
| [OpenMemory (Mem0)](https://mem0.ai/blog/introducing-openmemory-mcp) | Local store with dashboard; hosted version available | Local-first, one user's tools | Not described | Not described | See vendor |
| [mcp-vector-memory](https://github.com/BBerthod/mcp-vector-memory) | Qdrant + Ollama embeddings | Shared server for a team's code and docs | Not described | Not described in the summary reviewed | MIT |

Sources for the rows without a fetched README (OpenMemory, mcp-vector-memory) are search-result summaries and vendor posts, so they are weaker evidence.

## Overlap

- Shared memory across Claude Code, Codex and other MCP clients is well covered. Several projects already do it, some over Streamable HTTP.
- Hashed per-agent tokens, local embeddings through Ollama, and SQLite or Postgres full-text search are common.
- A human gate on agent-written memory exists: global-agent-memory has an owner approval queue. Approval as a concept is not new.

## Gaps the proposal targets (hypotheses)

None of the projects reviewed documented all of the following together:

- Several human identities with scope-level roles in a hierarchy (organization → team → project) and inheritance that flows only downward.
- Permission checks enforced in the database (RLS), not only in application code.
- Revision-bound approval that MCP cannot finalize, so an agent cannot launder its own proposal into policy.
- Immutable revisions and lifecycle events with an operator-only git export.

These are gaps in the documentation reviewed, not proven gaps in the products. Basic Memory's team roles are the closest overlap on multi-user scoping and need a closer look before release.

## Implications for the pilot

- The pilot should record whether scope inheritance and the approval queue are actually used. If they are not, the simpler alternatives above (repository files, a single-owner server like global-agent-memory) may be good enough for a solo author.
- The pilot is one person on several machines, so it can show cross-client and cross-machine continuity. It cannot show that teams want the multi-identity features.
- Stage 07 should refresh this table, especially Basic Memory's team features and mcp-memory-service's OAuth story.

No stack or scope change is proposed as a result of this review.
