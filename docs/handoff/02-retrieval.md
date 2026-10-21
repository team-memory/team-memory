# Handoff: stage 02 (intelligent retrieval)

Date: 2026-10-21. Branch: `claude/intelligent-retrieval-prompt-3c5570` (worktree). Nothing is committed or pushed yet.

## Summary

Hybrid retrieval is in place. Every new revision enqueues a durable embedding job in the same transaction. A worker embeds with Ollama and retries, backs off and dead-letters failed jobs. `memory_search` fuses PostgreSQL full-text and pgvector candidates with Reciprocal Rank Fusion, filtered to the reader's scope chain before ranking. It applies bounded trust boosts and a precedence band, excludes superseded and effectively overridden memories by default, and reports provenance on every result. When the query cannot be embedded, it falls back to full-text and says so.

**Headline numbers** (synthetic data; see the limits below):

- **Retrieval quality.** 67 memories and 42 labeled queries, with `nomic-embed-text` (the default, similarity floor 0.60):
  - Hybrid: recall@5 0.917, MRR 0.795. MRR varied by up to 0.06 between runs because of tie-breaking.
  - Full-text only: 0.893 / 0.689.
  - Vector only: 0.810 / 0.496.
  - Stage 01's all-terms full-text: 0.560 / 0.595.
  
  `qwen3-embedding:0.6b` (floor 0.50) reached 0.964 / 0.905 in hybrid. Details: `docs/eval/retrieval-report.md`.
- **Vector index.** At 20k, 100k and 250k embeddings, the chosen settings gave recall@50 = 1.0 for every scope-filtered case except one: a depth-8 chain covering 7.4% of 250k rows, at 0.953. p95 stayed at or below 15.6 ms (database time, excluding query embedding). With pgvector's default settings, the planner sometimes chose a post-filtered HNSW scan that returned 2–6% of the right rows. Details: `docs/decisions/vector-index-strategy.md`.
- **Conflict-candidate detector.** At the shipped thresholds: precision 0.83 / recall 0.63 (nomic, 0.75) and 0.86 / 0.75 (qwen, 0.70). These rest on 4 to 7 detections, so treat them as anecdotes.

## Acceptance criteria

| Criterion | Evidence | Result |
| --- | --- | --- |
| Creating a memory results in an embedding job that completes | `TestRevisionsEnqueueEmbeddingJobs` (fake provider). Live run on 2026-10-21 with the real binaries and your Ollama (`nomic-embed-text`): `init`, then `serve` and `worker`, then `memory_create` over MCP. The job succeeded within about 1 s, and a paraphrased `memory_search` returned the memory from both retrievers (`mode=hybrid`, similarity 0.708). | Pass |
| Stopping Ollama causes retries, then dead-lettering, never data loss; search still works via full-text | `TestWorkerRetriesThenDeadLetters`, `TestOllamaStoppedDeadLettersAndSearchFallsBack` (real Ollama client against a stopped server), `TestQueryEmbeddingTimeoutFallsBack`. Live run with `serve` and `worker` pointed at a dead endpoint: 9 retries with backoff, dead-lettered at the 10th attempt, memory row intact, `serve` logged the query-embedding failures and fell back. `jobs retry-dead` plus a healthy worker then completed the job. The live run did not capture the MCP response metadata; the tests assert `meta.mode=fulltext_only`. | Pass |
| Hybrid search returns provenance and trust fields; superseded excluded by default | `TestSearchProvenanceAndScopeChain`, `TestSupersededExcludedByDefault` (all three modes), `TestMCPProtocol`, `TestMCPSearchContract` | Pass |
| Eval report shows measured recall@5/MRR for full-text, vector and hybrid, with the dataset described; doc claims match the numbers | `docs/eval/retrieval-report.md`, `docs/eval/results-*.json`, `integration/eval/dataset.json` | Pass, with the stated limits: synthetic set written by the implementer, thresholds tuned on the same queries |
| Index strategy backed by measured recall/latency at stated sizes | `docs/decisions/vector-index-strategy.md`, `docs/eval/vectorbench-results.json` (20k / 100k / 250k) | Pass. One caveat: the run predates the `mem.scope_id = e.scope_id` join predicate added after review, and was not re-measured with it. |
| All tests pass against real PostgreSQL | `go test -race -count=1 ./...` with `TEST_DATABASE_URL`: 31 integration tests plus unit tests | Pass locally. CI has still never run, because nothing has been pushed. |

## Schema changes (`migrations/00002_retrieval.sql`)

- `embedding_models (name, dimensions, enabled)`: one row per embedding space. A trigger forbids changing name or dimensions. Seeded with `nomic-embed-text` / 768.
- `memory_embeddings.embedding` is now untyped `vector`. A composite FK `(model, dimensions) → embedding_models` and `CHECK (vector_dims(embedding) = dimensions)` keep rows consistent with their model.
- Indexes: `memory_embeddings (model, scope_id)` btree, and a partial HNSW per model, named with a hash of the model name (`memory_embeddings_nomic_embed_text_a4134b0e_hnsw` on `(embedding::vector(768)) vector_cosine_ops WHERE model = 'nomic-embed-text'`).
- `conflict_candidates`: suggested pairs (deeper memory, ancestor-scope memory, model, similarity, status `open|dismissed|linked`), with tenant columns for RLS. Stage 05's review queue consumes it.
- `jobs`: a new `lease_id uuid` column (a per-claim token that outcome updates must present), plus indexes for expired leases and dead jobs.
- `SearchMemories` (stage 01 full-text query) was removed. Its replacements are in `internal/postgres/queries/search.sql`.

## Code layout

- `internal/embeddings`: the `EmbeddingProvider` interface, `Ollama` (`/api/embed`, `truncate: true`), a deterministic `Fake`, model defaults (dimensions, task prefixes, thresholds), the per-model SQL fragments, `AddModel`, and the `Indexer` (embedding job handler plus conflict detection).
- `internal/jobs`: the generic PostgreSQL job worker. `FOR UPDATE SKIP LOCKED` claims with a lease, exponential backoff with ±20% jitter, dead-lettering at `max_attempts`, a lease reaper, and a `Metrics` hook interface (no-op default; Prometheus is stage 06).
- `internal/search`: hybrid search (`search.go`) and pure ranking functions (`rank.go`, unit-tested).
- `internal/testdb`: the per-test database helper, moved out of `integration/` so the eval and benchmark can use it.
- `cmd/team-memory/retrieval.go`: env configuration and the new subcommands `worker`, `embeddings add-model|list|enable|disable`, `jobs status|retry-dead`. The prompt named `cmd/worker`. A subcommand of the single binary follows the stage 01 decision recorded in `01-foundation.md`.
- `integration/eval/` (dataset and harness) and `integration/vectorbench/` (benchmark). Both are skipped unless their env vars are set.

## Configuration

See the README table. Summary: `TEAM_MEMORY_OLLAMA_URL` (unset means full-text only), `TEAM_MEMORY_EMBEDDING_MODEL` (default `nomic-embed-text`), `TEAM_MEMORY_EMBEDDING_DIMENSIONS`, `..._QUERY_PREFIX` / `..._DOCUMENT_PREFIX`, `TEAM_MEMORY_VECTOR_MIN_SIMILARITY`, `TEAM_MEMORY_CONFLICT_SIMILARITY`, `TEAM_MEMORY_QUERY_EMBED_TIMEOUT` (2s), `TEAM_MEMORY_HNSW_ITERATIVE_SCAN` / `TEAM_MEMORY_HNSW_EF_SEARCH`, and `TEAM_MEMORY_WORKER_*` (batch, poll interval, lease, backoff). No hostname is hardcoded. The evaluation's Ollama URL was supplied by environment only.

## Decisions recorded

- `docs/decisions/embedding-models.md`: the model registry, untyped vectors with a per-model partial HNSW index, how a second model is introduced, and why `nomic-embed-text` stays the default.
- `docs/decisions/vector-index-strategy.md`: a global per-model HNSW plus a `(model, scope_id)` btree, with the planner choosing per query under `force_custom_plan`, `hnsw.iterative_scan = relaxed_order` and `ef_search = 100`.
- `docs/decisions/retrieval-scoring.md`: RRF with k = 60, the bounded trust boost (+10% approved, +5% cited, decisions and conventions only, capped at 10%), the 0–1 score formula (not calibrated), the 5% precedence band, and the effective-override rule.

## Review

A read-only review subagent checked the stage 02 changes. All eight findings were confirmed and fixed. Findings 1, 2, 4, 5 and 7 have regression tests. Findings 3, 6 and 8 do not: 3 (snapshot isolation) and 8 (scope consistency) are races or states that cannot currently be produced, and 6 is a renamed and redocumented field, asserted in `TestMCPSearchContract`.

1. Model key prefixes could collide: `embed:<model>:` matched `nomic-embed-text:v1.5`. Fixed with a `|` delimiter.
2. Leases belonged to a worker *name*. Each claim now gets a `lease_id`.
3. Search used READ COMMITTED. Read transactions are now REPEATABLE READ.
4. A stale revision's vector could be stored. The revision is now rechecked under `FOR SHARE`.
5. Index names could collide, and an invalid index was silently accepted. Names are now hashed and validity is checked.
6. The `overridden_excluded` count was misleading. It is now `overrides_in_effect`.
7. Disabled models were still served and backfilled.
8. The vector query trusted the copied `scope_id`.

The reviewer's test-gap list was also addressed:
- the precedence test now exercises reordering;
- the override tests cover an overriding memory that is archived, superseded, or owned by a sibling scope, and a cross-organization target;
- the MCP contract test covers the `project` and `kinds` aliases and both include flags.

Two of the fixes were mutation-checked: removing the fix makes its new test fail.

## For stage 04 (security), in particular RLS and vector queries

- **RLS on `memory_embeddings`, `conflict_candidates` and `jobs`.** All three carry `org_id`, and the first two also carry `scope_id`, so policies can mirror `memories` without joins.
- **Re-run the benchmark with RLS on.** Use `make bench-vector` with policies enabled and a non-owner role. Policy predicates are added to the scan, which can change the planner's choice between the btree path and HNSW. The finding that matters most: under a filter, a non-iterative HNSW scan returns almost nothing. Check that `relaxed_order` still applies with policies in place, and that policy functions used in predicates are `LEAKPROOF` (or simple enough) not to block index use.
- **The search and the worker are separate trust paths.** Search filters to the scope chain in SQL and runs as the principal (`app.principal_id`). The worker runs operator-side (`uuid.Nil`) and reads every organization. In stage 04 it needs a dedicated role, or an RLS bypass limited to the embedding tables and the minimum columns of `memories` and `memory_revisions`.
- **Model DDL needs the owner role.** `embeddings add-model` builds indexes, so it must run with that role, like `migrate`. The application role must not have it.
- **Approvals feed search directly.** `EffectiveOverrides` reads `approvals`, `memories.trust` and `revision_overrides`. When stage 04/05 implement approval, rejection, archival and supersession, keep "latest outcome of the current revision" as the rule. Tests currently insert approvals by hand (`integration/retrieval_test.go`, function `approve`).
- **Promotion must keep embeddings consistent.** A promotion creates a new parent memory, and that gets its own job through the outbox. If anything ever changes `memories.scope_id` in place, the vector query's `mem.scope_id = e.scope_id` predicate drops that memory from vector results until it is re-embedded. That fails safe, but it degrades recall.
- **Rate-limit search.** Query embedding is per request to Ollama.
- **Purge.** The stage 06 purge runbook must cover `memory_embeddings`, `conflict_candidates` and `jobs.payload`.

## Known weaknesses

- **Retrieval quality is measured only on a small synthetic set** that I wrote, with thresholds tuned on the same queries. Real pilot queries may differ substantially. MRR (about 0.8) shows the right answer is often not first.
- **Embeddings are weak on code identifiers and acronyms** with `nomic-embed-text`; full-text carries those. `qwen3-embedding:0.6b` was clearly better here, but it was not adopted on this evidence (see the report).
- **Scores are rank-based and only meaningful within one response.** A query with no good answer can still produce a top score near 1. `vector_similarity` is the more absolute signal.
- **The conflict detector finds same-topic pairs, not disagreements.** Elaborations of a parent rule score as high as real conflicts. It also never compares different kinds (a decision against a convention).
- **Long memories are truncated by Ollama** at the model's context length before embedding.
- **One serve process uses one model.** Switching models means backfill, then switching serve, then disabling the old model.
- **Job rows are never deleted.** Succeeded jobs accumulate. Add a retention sweep in stage 06.
- **Jobs of a disabled model, or one with no running worker, stay pending** indefinitely. `jobs status` shows them.
- **The worker polls** at a 1 s default interval. There is no LISTEN/NOTIFY.
- **Metrics hooks exist but are not wired** (stage 06).

## Skipped or not verified

- **CI has not run.** Nothing is committed or pushed.
- **The vector benchmark ran on Docker Desktop (Apple Silicon, 8 GB VM), not the Kubernetes target.** Absolute latencies will differ there. Its query predates one join predicate (see the acceptance table).
- **Some CLI commands were not exercised live:**
  - `embeddings add-model`, `enable` and `disable` (their code paths are covered by `AddModel` in tests and the eval);
  - `serve` with an unregistered model.
- **No real MCP client (Claude Code, Codex) exercised the new `memory_search` fields** in this stage. The go-sdk client tests did.
- **There are no tests at the scale of the benchmark** for the conflict detector's `Neighbors` query. It now uses the same vector settings as search.
- **The eval was re-run after the review fixes, at the shipped conflict thresholds.** The worker's stored candidates matched the offline computation (6 pairs for nomic, 7 for qwen).
- **The proposal's search-latency target is assessed only for the vector retriever on local hardware.** There is no load test (stage 06).
