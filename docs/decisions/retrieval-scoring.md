# Decision: hybrid ranking, score and precedence

Status: accepted (stage 02, 2026-10-21). Constants live in `internal/search/rank.go`; change both together.

## Pipeline

1. **Candidate set.** The visible set is the target scope plus its ancestors, never descendants, and only for principals who can read the target scope. Only current revisions of `active` memories are considered, plus `superseded` ones when `include_superseded` is set. `archived` memories are never considered. Scope, status, kind and override exclusion are SQL predicates of each retriever's query, so filtering happens **before** ranking. A candidate outside the chain can never take a ranking slot. The access check, the override check, both retrievers and the row fetch run in one read-only REPEATABLE READ transaction, so they all see the same snapshot. A memory archived mid-search cannot appear. The vector query also requires the embedding's scope to equal the memory's current scope.
2. **Effective overrides** are computed first on every search, across the whole chain, whether or not the overriding memory matches the query. An override is effective when the overriding memory is active, its scope is in the reader's chain and strictly below the overridden memory's scope, its trust is `approved`, and the **latest** review outcome of its **current** revision is an approval. The link is part of that revision, so approving the revision approves the link. Draft links never hide anything. Overridden memories are excluded from both retrievers, or kept and flagged with `overridden_by` when `include_overridden` is set. Nothing can produce an approval until stages 04/05; tests insert approvals directly.
3. **Retrievers.** Each contributes up to 50 candidates:
   - **Full-text:** `ts_rank_cd` over a weighted `tsvector` (title A, content B, rationale C). Queries without websearch operators have their lexemes OR-ed, so natural-language questions match documents that share some terms. Queries with `"phrases"`, `OR` or `-exclusions` keep strict `websearch_to_tsquery` semantics.
   - **Vector:** cosine similarity of the query embedding with the model's current-revision embeddings (see `vector-index-strategy.md`). Candidates below the model's `MinVectorSimilarity` are dropped, so unrelated queries do not return arbitrary nearest neighbours.
4. **Fusion.** Reciprocal Rank Fusion with *k* = 60: `rrf = Σ 1 / (60 + rank_r)` over the retrievers that returned the memory (ranks are 1-based).
5. **Bounded trust boost.** `fused = rrf × (1 + b)`, where *b* = 0.10 for `approved` and 0.05 for `cited` **decisions and conventions**, and 0 otherwise, capped at 0.10. Facts, lessons and context are not boosted. An approved convention gets at least the boost of a cited one, so the boost is monotonic in trust. Effect: within a single retriever's list, the boost can lift a memory over roughly six positions near the top (1.10/(60+r) > 1/61 ⇔ r < 7.1) and fewer further down. It cannot beat a memory that both retrievers ranked highly: two top-3 ranks give 2/63 ≈ 0.032, against at most 1.10/61 ≈ 0.018 from one list.
6. **Score.** `score = fused / max_fused`, where `max_fused = R / 61 × 1.10` and *R* is the number of retrievers that ran (2 for hybrid, 1 after a fallback). It lies in [0, 1], is monotonic in `fused`, and reaches 1 only for rank 1 in every retriever that ran with the maximum boost.
7. **Precedence.** Results are sorted by `fused`, then partitioned into bands. A band starts at its highest-scoring member and contains every following result within 5% of it (`fused ≥ leader / 1.05`). Inside a band, results from deeper scopes come first, then score. No result can pass another that out-scores it by more than the margin. Every result reports `precedence` = origin scope depth (0 = organization).

## What the score is not

RRF is rank-based. The score says how a memory placed **relative to the other candidates of this one query**. It is not a probability of relevance, it is not comparable across queries, and its scale differs between hybrid and fallback responses. Every response carries a `score_note` saying so. A query with no good answer can still produce a top score of 1.0, so `vector_similarity`, when present, is the more absolute signal. Calibration was not attempted. The evaluation set is far too small for it.

## Precedence is not conflict detection

Depth orders near-ties, and that is all it does. It does not claim that two results disagree, or that the deeper one is authoritative. Only an approved `overrides` link hides an inherited memory. Conflict *candidates* are a separate, stored suggestion list for the stage 05 review queue: same kind, ancestor/descendant scopes, cosine similarity at or above a threshold. They never change results. See `docs/eval/retrieval-report.md` for their measured precision.

## Response metadata

`meta.mode` is `hybrid`, `fulltext_only` or `vector_only` (evaluation only). `meta.vector.status` is `ok`, `disabled`, `skipped`, `timeout`, `unavailable` or `misconfigured`. Provider errors are logged server-side and replaced with a generic message, so internal hostnames never reach clients. `meta.vector.pending_embeddings` counts visible memories whose current revision has no embedding yet; they are reachable only through full-text. `meta.overrides_in_effect` counts memories in the reader's scope chain that an effective override hides, whether or not they matched the query.

## Alternatives considered

- **Weighted score blending** (normalized BM25 plus cosine). This needs per-corpus normalization and tuning that a small labeled set cannot support. RRF has one well-studied constant.
- **Depth as a score multiplier.** This would let a weakly relevant project memory outrank a strongly relevant organization rule without bound. The band rule caps the reordering.
- **Boosting all approved kinds.** The proposal singles out decisions and conventions, the kinds that act as team policy. Approved facts are still labeled `approved`; they just get no ranking advantage.
