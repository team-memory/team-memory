# Stage 02: Intelligent Retrieval

## Objective
Add durable embedding generation and hybrid (full-text + vector) search with Reciprocal Rank Fusion, bounded trust boosts, and provenance in every result — while keeping search functional when embeddings are pending or the model server is offline.

## Context
Team Memory is an open-source Go service giving AI agents a shared, versioned, permission-aware knowledge base on PostgreSQL 17+ with pgvector, exposed over MCP (see `proposal.md`). Stage 01 built migrations, memory lifecycle services with revisions, the MCP server, and full-text search. Embeddings are pluggable; local Ollama with a dedicated embedding model (`nomic-embed-text`, 768 dimensions, as the initial candidate) comes first. Embeddings are stored per `(memory_id, revision, model)` in a separate table so models can change without rewriting knowledge records.

## Inputs
- `prompts/00-start-here.md` (attach; follow its global rules and completion gates).
- `review-decisions.md` (attach with the proposal; required accepted security/lifecycle decisions).
- `proposal.md` and `docs/handoff/01-foundation.md` (attach). If unavailable, ask the user; do not guess the schema.
- The repository root (the repository root), which holds both the service code and these planning files.
- A reachable Ollama endpoint with an embedding model pulled (ask the user for the URL; use a config variable, never a hardcoded host).

## Work
1. `internal/embeddings`: `EmbeddingProvider` interface (`Embed(ctx, texts) ([][]float32, error)`), an Ollama implementation, and a deterministic fake for tests. Provider config declares model name and dimensions; validate returned dimensions and fail loudly on mismatch.
2. `internal/jobs` and `cmd/worker`: a PostgreSQL-backed job queue / transactional outbox. Memory create/update enqueues an embedding job in the same transaction. Worker claims jobs with `FOR UPDATE SKIP LOCKED`, retries with backoff, dead-letters after a max attempts, is idempotent per `(memory_id, revision, model)`, and exposes metrics hooks (full Prometheus wiring is stage 06).
3. Embedding storage: handle the model/dimension question explicitly. Decide and document how a second model is introduced (new table or column per dimension, new index) without mixing incompatible vectors. Record in `docs/decisions/embedding-models.md`.
4. `internal/search`: hybrid search over **current, active, authorized** memories (superseded excluded unless requested). The candidate set is the target scope plus its ancestors (the visible set from stage 01), never descendants; filter by scope before ranking, not after:
   - Query embedding via the configured provider; if it fails or times out, fall back to full-text only and say so in the result metadata.
   - Vector and full-text candidate sets, merged with RRF.
   - Bounded boost for `approved` decisions and `cited` conventions (cap the adjustment; document the formula).
   - Result fields: id, revision, kind, content, trust level, source_uri, updated_at, score, `origin_scope`, `inherited`, plus a flag showing which retrievers contributed. Conflict handling is **lowest scope wins**: memories overridden by an effective, revision-approved `overrides` link from a lower scope within the searcher's scope chain are excluded from default results (stage 04 activates approval; before then draft links never hide results) (included, flagged `overridden_by`, when `include_overridden` is set). Without an explicit link, apply a documented rule that ranks results from more specific scopes ahead of inherited ones when relevance scores are within a small bounded margin, and include a `precedence` field (scope depth) in every result as scope context, without claiming that depth alone establishes a conflict or authority. Add a conflict-candidate detector (same kind, high vector similarity, different scopes) that does not change search results but feeds suggestions for stage 05's review queue. Evaluate it on a small labeled set and report precision honestly.
5. Score honesty: RRF output is rank-based, not a calibrated probability. Define a documented, monotonic 0–1 relevance score, and do not claim calibration. Build a small labeled evaluation set (≥30 query/expected-memory pairs spanning code identifiers, architectural acronyms, repository names, paraphrases) in `integration/eval/` and report recall@5 and MRR for full-text only, vector only, and hybrid.
6. Vector index and filtering: benchmark HNSW with selective scope filters, including the ancestor-set filter (an `IN (scope ids)` over a small set) and deep trees, on a synthetic dataset (state the sizes used). Report recall and query plans; choose between a global index, iterative scan settings, partial indexes, or partitioning; record in `docs/decisions/vector-index-strategy.md`. Do not assume the global index is adequate.
7. Evaluate at least one alternative embedding model if available locally; record results but keep `nomic-embed-text` default unless the eval clearly favors another.
8. Update MCP `memory_search` to the contract: inputs `scope` (alias `project`), `include_overridden`, `query`, `types`, `limit`, `include_superseded`. Update resources if they use search.
9. Tests: worker retry and dead-letter behavior; idempotency; fallback to full-text; hybrid ordering; superseded exclusion.

## Excluded
OIDC, RLS policy enforcement, approval workflow, Helm, dashboards, automatic conversation extraction.

## Deliverables
Embedding provider and worker, hybrid search, eval set and report (`docs/eval/retrieval-report.md`), decision records, updated MCP tool, tests, `docs/handoff/02-retrieval.md`.

## Acceptance criteria
- Creating a memory results in an embedding job that completes; stopping Ollama causes retries then dead-letter, never data loss, and search still works via full-text.
- Hybrid search returns provenance and trust fields; superseded memories are excluded by default.
- The eval report shows measured recall@5/MRR for the three modes with the dataset described; claims in docs match the numbers.
- Index strategy decision is backed by measured recall/latency at stated dataset sizes.
- All tests pass against real PostgreSQL.

## Handoff
`docs/handoff/02-retrieval.md`: schema changes, config variables, measured results and their limits, known weaknesses, and anything stage 04 must account for (RLS interaction with vector queries in particular).
