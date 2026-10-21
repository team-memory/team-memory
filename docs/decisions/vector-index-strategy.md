# Decision: vector index strategy for scope-filtered search

Status: accepted (stage 02, 2026-10-21). Measurements: `docs/eval/vectorbench-results.json`. Harness: `integration/vectorbench/` (`make bench-vector`).

## Decision

Keep **one partial HNSW index per embedding model** (`vector_cosine_ops`, default build parameters m = 16 and ef_construction = 64), **plus a btree on `memory_embeddings (model, scope_id)`**, and let PostgreSQL choose between them per query. Every vector query runs with:

- `plan_cache_mode = force_custom_plan`. The plan is made for this request's scope list. A cached generic plan cannot know how selective the list is. This was not hypothetical: the first benchmark run silently reused one generic plan across strategies.
- `hnsw.iterative_scan = relaxed_order` and `hnsw.ef_search = 100`. When the planner does pick HNSW under a scope filter, the scan keeps going until it has found enough rows *inside* the filter. Results are re-sorted by similarity afterwards.

These settings are applied by `embeddings.ApplyVectorSettings`, used by search and by the conflict detector. Both are configurable (`TEAM_MEMORY_HNSW_ITERATIVE_SCAN`, `TEAM_MEMORY_HNSW_EF_SEARCH`).

**Not chosen at these sizes:** per-scope partial indexes, partitioning, and a global index alone. Revisit when the conditions under "When to revisit" are met.

## Setup

- **Data:** synthetic data loaded through the real schema (`memories`, `memory_revisions`, `memory_embeddings`), at **20,000, 100,000 and 250,000 embeddings** of 768 dimensions. Unit vectors are drawn around 256 topic centroids (noise 0.9 for documents, 0.6 for queries).
- **Topics vs scopes:** topics are independent of scopes, so a filtered query's nearest neighbours are mostly *outside* the filter. That is the hard case for post-filtering.
- **Tree:** one large organization with 20 teams × 10 projects, plus a chain of groups down to depth 8. Nine small organizations hold 5% of the rows.
- **Scenarios:** the reader's visible set is the target scope plus its ancestors (`e.scope_id = ANY(...)`). Shares of rows: organization root 2%, team chain 2.4%, project chain 2.8%, depth-8 chain 7.4%, a small organization's project chain 0.33%, and an unfiltered reference.
- **Query shape:** the production query (join to `memories` on the current revision, `status = 'active'`, `LIMIT 50`), with 50 queries per cell. The ground truth is the exact scan.
  - recall@50 = overlap of the returned 50 with the exact 50.
  - recall@10 = share of the exact top 10 found in the returned top 10.
  - min rows = the fewest rows any query returned.
- **Strategies:**
  - exact (ORDER BY rewritten so the index cannot be used);
  - planner choice with iterative scan off at ef 40 (pgvector defaults);
  - planner choice with relaxed_order at ef 100 (**chosen**);
  - HNSW forced (`enable_bitmapscan/seqscan/sort = off`) with iterative scan off at ef 40 and 200, relaxed_order at ef 100, and strict_order at ef 100.
- **Environment:** PostgreSQL 17 with pgvector 0.8.1 in Docker Desktop on an Apple Silicon Mac (8 GB VM), warm cache, client on the same host. Latency is database time plus a loopback round trip, excluding query embedding. Absolute numbers will differ on the Kubernetes target. The relative behaviour is the point.
- **Build:** the HNSW index was rebuilt after bulk load with `maintenance_work_mem = 2GB` and serial build: 7.7 s / 78 MB at 20k, 33 s / 391 MB at 100k, 2 min 4 s / 977 MB at 250k.
- **Caveat:** this run predates one predicate added after review, `mem.scope_id = e.scope_id` in the join, which the harness now includes. It is a join filter on rows already fetched by primary key and is not expected to change plan choice, but these numbers were not re-measured with it.

## Findings

1. **Post-filtered HNSW without iterative scan fails badly.** With pgvector's defaults, a filtered HNSW scan returns at most `ef_search` rows *before* filtering. Measured recall@50 was 0.002–0.06 for 0.3–7.4% filters, often with **zero rows returned**. Raising ef_search to 200 only reaches 0.07–0.29.
2. **The planner usually avoids that trap, but not always.** At pgvector's defaults it picked a non-iterative HNSW scan for the depth-8 chain at 100k and 250k, and for the project chain at 250k. Those queries returned 2–6% of the right rows (recall@50 0.023–0.059), sometimes none. A global index with default settings is therefore **not adequate**. This is the strongest reason to set `iterative_scan`.
3. **With the chosen settings, the planner picks an exact filtered scan for every scope chain except the depth-8 chain at 250k**, giving recall 1.0. That means the btree on `(model, scope_id)`, or a sequential scan of the narrow heap (vectors are TOASTed), then a sort. p95 was at most 5.4 ms at 20k and 100k (10.2 ms for the depth-8 chain at 100k) and 4.5–14.5 ms at 250k.
4. **When it picks HNSW, iterative scan keeps recall high.** That happened for the 7.4% depth-8 chain at 250k: recall@50 0.953 at p95 8.9 ms, versus 1.0 at p95 23.6 ms for the exact scan. That is the one scope-filtered cell with recall below 1.0 under the chosen settings.
5. **Forced HNSW with iterative scan is slower than the exact scan for selective filters** (15–60 ms) and loses recall for very selective ones: 0.48–0.59 for the 0.33% small-organization chain. pgvector stops after `hnsw.max_scan_tuples` (20,000 by default). Leaving the choice to the planner avoids this.
6. **Unfiltered or barely filtered, HNSW wins:** 0.97–0.98 recall@50 at p95 3–16 ms, against 114 ms (100k) and 257 ms p50 (250k) for an exact scan.
7. **All chosen-strategy cells meet the proposal's target** (search p95 < 500 ms warmed, excluding query embedding) for the vector retriever on this hardware. Full-text and fusion add little: about 2 ms in the retrieval eval.

## When to revisit

- **A large tenant's visible chain covers several percent of an index past a few hundred thousand rows.** Recall then depends on the HNSW path (finding 4). Options in order: raise `hnsw.ef_search` and `hnsw.max_scan_tuples`; then **list partitioning of `memory_embeddings` by `org_id`** (or per-organization partial indexes for the largest tenants), so each organization's index only holds its own rows.
- **Vectors of 1,024+ dimensions at the same sizes.** Consider `halfvec` storage.
- **Stage 04 enables RLS.** Re-run this benchmark with policies on and a non-owner role, because policy predicates change both plans and costs. See the stage 02 handoff.
- **Parallel index builds in containers** allocate `maintenance_work_mem` in dynamic shared memory. Docker's default 64 MB `/dev/shm` made a 2 GB parallel build fail during this benchmark. Size `/dev/shm` (Kubernetes: an `emptyDir` with `medium: Memory`) or build serially. This is a stage 06 runbook item.

## Measurements

The tables are generated from `vectorbench-results.json`. "Plan" is the scan PostgreSQL chose for the first query of each cell.

#### 20,000 embeddings

| Scenario (share of rows) | Strategy | Plan | recall@10 | recall@50 | min rows | p50 ms | p95 ms |
| --- | --- | --- | --- | --- | --- | --- | --- |
| org root (depth 0) (2.00%) | exact | btree + sort | 1.000 | 1.000 | 50 | 2.27 | 2.62 |
| org root (depth 0) (2.00%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 2.23 | 2.55 |
| org root (depth 0) (2.00%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 2.40 | 2.68 |
| org root (depth 0) (2.00%) | HNSW, iter off, ef 40 | HNSW | 0.066 | 0.013 | 0 | 0.82 | 1.08 |
| org root (depth 0) (2.00%) | HNSW, iter off, ef 200 | HNSW | 0.438 | 0.088 | 0 | 2.07 | 3.27 |
| org root (depth 0) (2.00%) | HNSW, relaxed, ef 100 | HNSW | 0.966 | 0.996 | 50 | 13.48 | 17.37 |
| org root (depth 0) (2.00%) | HNSW, strict, ef 100 | HNSW | 0.928 | 0.958 | 50 | 14.88 | 36.03 |
| team (depth 1) (2.40%) | exact | btree + sort | 1.000 | 1.000 | 50 | 2.27 | 2.49 |
| team (depth 1) (2.40%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 2.69 | 4.96 |
| team (depth 1) (2.40%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 2.88 | 4.26 |
| team (depth 1) (2.40%) | HNSW, iter off, ef 40 | HNSW | 0.084 | 0.017 | 0 | 0.77 | 1.15 |
| team (depth 1) (2.40%) | HNSW, iter off, ef 200 | HNSW | 0.510 | 0.102 | 1 | 3.03 | 6.91 |
| team (depth 1) (2.40%) | HNSW, relaxed, ef 100 | HNSW | 0.976 | 0.991 | 50 | 12.58 | 22.19 |
| team (depth 1) (2.40%) | HNSW, strict, ef 100 | HNSW | 0.938 | 0.953 | 50 | 13.82 | 20.68 |
| project (depth 2) (2.80%) | exact | btree + sort | 1.000 | 1.000 | 50 | 3.08 | 5.83 |
| project (depth 2) (2.80%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 2.92 | 4.55 |
| project (depth 2) (2.80%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 2.61 | 3.12 |
| project (depth 2) (2.80%) | HNSW, iter off, ef 40 | HNSW | 0.106 | 0.021 | 0 | 0.79 | 1.08 |
| project (depth 2) (2.80%) | HNSW, iter off, ef 200 | HNSW | 0.592 | 0.120 | 1 | 1.98 | 2.63 |
| project (depth 2) (2.80%) | HNSW, relaxed, ef 100 | HNSW | 0.968 | 0.992 | 50 | 10.29 | 12.43 |
| project (depth 2) (2.80%) | HNSW, strict, ef 100 | HNSW | 0.940 | 0.950 | 50 | 10.17 | 11.97 |
| deep leaf (depth 8) (7.37%) | exact | btree + sort | 1.000 | 1.000 | 50 | 4.35 | 6.33 |
| deep leaf (depth 8) (7.37%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 3.88 | 6.27 |
| deep leaf (depth 8) (7.37%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 3.61 | 4.21 |
| deep leaf (depth 8) (7.37%) | HNSW, iter off, ef 40 | HNSW | 0.292 | 0.058 | 0 | 0.75 | 0.99 |
| deep leaf (depth 8) (7.37%) | HNSW, iter off, ef 200 | HNSW | 0.908 | 0.282 | 8 | 2.47 | 3.52 |
| deep leaf (depth 8) (7.37%) | HNSW, relaxed, ef 100 | HNSW | 0.914 | 0.886 | 50 | 6.49 | 10.62 |
| deep leaf (depth 8) (7.37%) | HNSW, strict, ef 100 | HNSW | 0.874 | 0.752 | 50 | 6.86 | 10.27 |
| small-org project (0.33%) | exact | btree + sort | 1.000 | 1.000 | 50 | 2.33 | 5.79 |
| small-org project (0.33%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 2.80 | 8.80 |
| small-org project (0.33%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 2.26 | 3.71 |
| small-org project (0.33%) | HNSW, iter off, ef 40 | HNSW | 0.014 | 0.003 | 0 | 0.90 | 1.64 |
| small-org project (0.33%) | HNSW, iter off, ef 200 | HNSW | 0.074 | 0.015 | 0 | 3.82 | 5.79 |
| small-org project (0.33%) | HNSW, relaxed, ef 100 | HNSW | 0.970 | 0.999 | 50 | 35.21 | 50.78 |
| small-org project (0.33%) | HNSW, strict, ef 100 | HNSW | 0.794 | 0.928 | 50 | 33.04 | 37.60 |
| unfiltered reference (100.00%) | exact | seq scan + sort | 1.000 | 1.000 | 50 | 27.22 | 31.42 |
| unfiltered reference (100.00%) | planner, iter off, ef 40 | HNSW | 1.000 | 0.800 | 40 | 1.94 | 2.87 |
| unfiltered reference (100.00%) | **planner, relaxed, ef 100 (chosen)** | HNSW | 1.000 | 1.000 | 50 | 2.21 | 2.71 |
| unfiltered reference (100.00%) | HNSW, iter off, ef 40 | HNSW | 1.000 | 0.800 | 40 | 1.24 | 1.57 |
| unfiltered reference (100.00%) | HNSW, iter off, ef 200 | HNSW | 1.000 | 1.000 | 50 | 2.57 | 4.72 |
| unfiltered reference (100.00%) | HNSW, relaxed, ef 100 | HNSW | 1.000 | 1.000 | 50 | 1.53 | 1.78 |
| unfiltered reference (100.00%) | HNSW, strict, ef 100 | HNSW | 1.000 | 1.000 | 50 | 1.53 | 1.98 |

#### 100,000 embeddings

| Scenario (share of rows) | Strategy | Plan | recall@10 | recall@50 | min rows | p50 ms | p95 ms |
| --- | --- | --- | --- | --- | --- | --- | --- |
| org root (depth 0) (2.00%) | exact | btree + sort | 1.000 | 1.000 | 50 | 3.32 | 3.70 |
| org root (depth 0) (2.00%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 3.31 | 3.56 |
| org root (depth 0) (2.00%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 3.38 | 4.18 |
| org root (depth 0) (2.00%) | HNSW, iter off, ef 40 | HNSW | 0.060 | 0.012 | 0 | 1.18 | 1.48 |
| org root (depth 0) (2.00%) | HNSW, iter off, ef 200 | HNSW | 0.370 | 0.074 | 0 | 1.46 | 2.25 |
| org root (depth 0) (2.00%) | HNSW, relaxed, ef 100 | HNSW | 0.934 | 0.917 | 50 | 20.11 | 25.50 |
| org root (depth 0) (2.00%) | HNSW, strict, ef 100 | HNSW | 0.904 | 0.808 | 50 | 22.90 | 32.16 |
| team (depth 1) (2.40%) | exact | btree + sort | 1.000 | 1.000 | 50 | 3.99 | 4.92 |
| team (depth 1) (2.40%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 3.67 | 4.04 |
| team (depth 1) (2.40%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 3.70 | 4.32 |
| team (depth 1) (2.40%) | HNSW, iter off, ef 40 | HNSW | 0.072 | 0.014 | 0 | 1.07 | 1.36 |
| team (depth 1) (2.40%) | HNSW, iter off, ef 200 | HNSW | 0.442 | 0.088 | 0 | 1.44 | 2.29 |
| team (depth 1) (2.40%) | HNSW, relaxed, ef 100 | HNSW | 0.962 | 0.906 | 50 | 18.32 | 23.38 |
| team (depth 1) (2.40%) | HNSW, strict, ef 100 | HNSW | 0.940 | 0.806 | 50 | 19.28 | 25.24 |
| project (depth 2) (2.80%) | exact | btree + sort | 1.000 | 1.000 | 50 | 4.25 | 4.74 |
| project (depth 2) (2.80%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 4.23 | 5.08 |
| project (depth 2) (2.80%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 4.15 | 4.48 |
| project (depth 2) (2.80%) | HNSW, iter off, ef 40 | HNSW | 0.090 | 0.018 | 0 | 1.10 | 1.44 |
| project (depth 2) (2.80%) | HNSW, iter off, ef 200 | HNSW | 0.520 | 0.104 | 0 | 1.45 | 2.00 |
| project (depth 2) (2.80%) | HNSW, relaxed, ef 100 | HNSW | 0.968 | 0.899 | 50 | 15.46 | 20.31 |
| project (depth 2) (2.80%) | HNSW, strict, ef 100 | HNSW | 0.954 | 0.803 | 50 | 16.70 | 22.98 |
| deep leaf (depth 8) (7.40%) | exact | btree + sort | 1.000 | 1.000 | 50 | 9.05 | 9.34 |
| deep leaf (depth 8) (7.40%) | planner, iter off, ef 40 | HNSW | 0.288 | 0.059 | 0 | 1.11 | 1.36 |
| deep leaf (depth 8) (7.40%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 9.15 | 10.24 |
| deep leaf (depth 8) (7.40%) | HNSW, iter off, ef 40 | HNSW | 0.288 | 0.059 | 0 | 1.11 | 1.35 |
| deep leaf (depth 8) (7.40%) | HNSW, iter off, ef 200 | HNSW | 0.980 | 0.278 | 7 | 1.52 | 2.15 |
| deep leaf (depth 8) (7.40%) | HNSW, relaxed, ef 100 | HNSW | 0.966 | 0.868 | 50 | 6.47 | 8.72 |
| deep leaf (depth 8) (7.40%) | HNSW, strict, ef 100 | HNSW | 0.966 | 0.781 | 50 | 7.30 | 10.18 |
| small-org project (0.33%) | exact | seq scan + sort | 1.000 | 1.000 | 50 | 4.98 | 5.17 |
| small-org project (0.33%) | planner, iter off, ef 40 | seq scan + sort | 1.000 | 1.000 | 50 | 5.16 | 5.67 |
| small-org project (0.33%) | **planner, relaxed, ef 100 (chosen)** | seq scan + sort | 1.000 | 1.000 | 50 | 5.04 | 5.40 |
| small-org project (0.33%) | HNSW, iter off, ef 40 | HNSW | 0.010 | 0.002 | 0 | 1.05 | 1.33 |
| small-org project (0.33%) | HNSW, iter off, ef 200 | HNSW | 0.060 | 0.012 | 0 | 1.41 | 2.07 |
| small-org project (0.33%) | HNSW, relaxed, ef 100 | HNSW | 0.800 | 0.590 | 45 | 37.59 | 39.80 |
| small-org project (0.33%) | HNSW, strict, ef 100 | HNSW | 0.580 | 0.530 | 42 | 38.14 | 42.77 |
| unfiltered reference (100.00%) | exact | seq scan + sort | 1.000 | 1.000 | 50 | 113.67 | 118.74 |
| unfiltered reference (100.00%) | planner, iter off, ef 40 | HNSW | 0.936 | 0.752 | 40 | 2.05 | 2.81 |
| unfiltered reference (100.00%) | **planner, relaxed, ef 100 (chosen)** | HNSW | 0.980 | 0.978 | 50 | 2.31 | 3.32 |
| unfiltered reference (100.00%) | HNSW, iter off, ef 40 | HNSW | 0.936 | 0.752 | 40 | 1.93 | 2.88 |
| unfiltered reference (100.00%) | HNSW, iter off, ef 200 | HNSW | 1.000 | 0.998 | 50 | 2.26 | 3.13 |
| unfiltered reference (100.00%) | HNSW, relaxed, ef 100 | HNSW | 0.980 | 0.978 | 50 | 2.06 | 3.12 |
| unfiltered reference (100.00%) | HNSW, strict, ef 100 | HNSW | 0.980 | 0.978 | 50 | 2.13 | 2.61 |

#### 250,000 embeddings

| Scenario (share of rows) | Strategy | Plan | recall@10 | recall@50 | min rows | p50 ms | p95 ms |
| --- | --- | --- | --- | --- | --- | --- | --- |
| org root (depth 0) (2.00%) | exact | btree + sort | 1.000 | 1.000 | 50 | 6.63 | 9.83 |
| org root (depth 0) (2.00%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 5.95 | 7.52 |
| org root (depth 0) (2.00%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 7.10 | 8.64 |
| org root (depth 0) (2.00%) | HNSW, iter off, ef 40 | HNSW | 0.074 | 0.015 | 0 | 4.08 | 7.18 |
| org root (depth 0) (2.00%) | HNSW, iter off, ef 200 | HNSW | 0.366 | 0.073 | 0 | 4.50 | 6.91 |
| org root (depth 0) (2.00%) | HNSW, relaxed, ef 100 | HNSW | 0.996 | 0.866 | 50 | 33.08 | 55.28 |
| org root (depth 0) (2.00%) | HNSW, strict, ef 100 | HNSW | 0.996 | 0.780 | 50 | 32.40 | 47.33 |
| team (depth 1) (2.40%) | exact | btree + sort | 1.000 | 1.000 | 50 | 7.57 | 10.86 |
| team (depth 1) (2.40%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 9.94 | 13.61 |
| team (depth 1) (2.40%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 8.30 | 14.53 |
| team (depth 1) (2.40%) | HNSW, iter off, ef 40 | HNSW | 0.096 | 0.019 | 0 | 2.96 | 4.21 |
| team (depth 1) (2.40%) | HNSW, iter off, ef 200 | HNSW | 0.456 | 0.091 | 0 | 4.56 | 17.49 |
| team (depth 1) (2.40%) | HNSW, relaxed, ef 100 | HNSW | 0.998 | 0.867 | 50 | 28.12 | 39.14 |
| team (depth 1) (2.40%) | HNSW, strict, ef 100 | HNSW | 0.998 | 0.785 | 50 | 28.45 | 47.59 |
| project (depth 2) (2.80%) | exact | btree + sort | 1.000 | 1.000 | 50 | 8.94 | 14.40 |
| project (depth 2) (2.80%) | planner, iter off, ef 40 | HNSW | 0.112 | 0.023 | 0 | 3.05 | 6.27 |
| project (depth 2) (2.80%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 8.83 | 12.28 |
| project (depth 2) (2.80%) | HNSW, iter off, ef 40 | HNSW | 0.112 | 0.023 | 0 | 2.10 | 2.72 |
| project (depth 2) (2.80%) | HNSW, iter off, ef 200 | HNSW | 0.550 | 0.112 | 0 | 3.31 | 4.17 |
| project (depth 2) (2.80%) | HNSW, relaxed, ef 100 | HNSW | 0.998 | 0.878 | 50 | 19.69 | 30.08 |
| project (depth 2) (2.80%) | HNSW, strict, ef 100 | HNSW | 0.998 | 0.806 | 50 | 22.25 | 33.07 |
| deep leaf (depth 8) (7.40%) | exact | btree + sort | 1.000 | 1.000 | 50 | 17.99 | 23.62 |
| deep leaf (depth 8) (7.40%) | planner, iter off, ef 40 | HNSW | 0.246 | 0.049 | 0 | 3.73 | 7.09 |
| deep leaf (depth 8) (7.40%) | **planner, relaxed, ef 100 (chosen)** | HNSW | 0.952 | 0.953 | 50 | 6.38 | 8.94 |
| deep leaf (depth 8) (7.40%) | HNSW, iter off, ef 40 | HNSW | 0.246 | 0.049 | 0 | 1.97 | 2.51 |
| deep leaf (depth 8) (7.40%) | HNSW, iter off, ef 200 | HNSW | 0.930 | 0.292 | 7 | 3.36 | 5.26 |
| deep leaf (depth 8) (7.40%) | HNSW, relaxed, ef 100 | HNSW | 0.952 | 0.953 | 50 | 4.67 | 11.29 |
| deep leaf (depth 8) (7.40%) | HNSW, strict, ef 100 | HNSW | 0.948 | 0.950 | 50 | 5.86 | 8.61 |
| small-org project (0.33%) | exact | btree + sort | 1.000 | 1.000 | 50 | 2.70 | 4.87 |
| small-org project (0.33%) | planner, iter off, ef 40 | btree + sort | 1.000 | 1.000 | 50 | 2.36 | 3.04 |
| small-org project (0.33%) | **planner, relaxed, ef 100 (chosen)** | btree + sort | 1.000 | 1.000 | 50 | 2.93 | 4.49 |
| small-org project (0.33%) | HNSW, iter off, ef 40 | HNSW | 0.016 | 0.003 | 0 | 2.41 | 3.06 |
| small-org project (0.33%) | HNSW, iter off, ef 200 | HNSW | 0.066 | 0.013 | 0 | 3.74 | 5.12 |
| small-org project (0.33%) | HNSW, relaxed, ef 100 | HNSW | 0.742 | 0.484 | 46 | 59.63 | 83.78 |
| small-org project (0.33%) | HNSW, strict, ef 100 | HNSW | 0.568 | 0.430 | 44 | 51.91 | 59.47 |
| unfiltered reference (100.00%) | exact | seq scan + sort | 1.000 | 1.000 | 50 | 257.37 | 434.37 |
| unfiltered reference (100.00%) | planner, iter off, ef 40 | HNSW | 0.898 | 0.768 | 40 | 6.39 | 8.27 |
| unfiltered reference (100.00%) | **planner, relaxed, ef 100 (chosen)** | HNSW | 0.962 | 0.969 | 50 | 7.25 | 15.62 |
| unfiltered reference (100.00%) | HNSW, iter off, ef 40 | HNSW | 0.898 | 0.768 | 40 | 3.70 | 5.19 |
| unfiltered reference (100.00%) | HNSW, iter off, ef 200 | HNSW | 0.970 | 0.974 | 50 | 4.95 | 7.98 |
| unfiltered reference (100.00%) | HNSW, relaxed, ef 100 | HNSW | 0.962 | 0.969 | 50 | 4.26 | 5.11 |
| unfiltered reference (100.00%) | HNSW, strict, ef 100 | HNSW | 0.962 | 0.969 | 50 | 3.57 | 4.89 |
