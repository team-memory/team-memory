# Retrieval evaluation report (stage 02)

Date: 2026-10-21. Raw results: `results-nomic-embed-text.json` and `results-qwen3-embedding-0.6b.json` in this directory. Harness: `integration/eval/` (`make eval`).

## What was measured, and what it is worth

- **Dataset:** `integration/eval/dataset.json`. It holds 67 synthetic engineering memories in 8 scopes (one organization, three teams, four projects) and 42 labeled queries: 9 code identifiers, 11 acronyms, 5 repository names and 17 paraphrases. 5 of the queries have two relevant memories. There are also 5 unanswerable queries, 8 labeled conflict pairs and 8 hard-negative pairs. 8 decisions and conventions are approved (inserted directly, standing in for stage 05 review) so the trust boost is exercised.
- **Provenance:** I wrote the corpus and the labels myself, and I was also the person who built the system. That favours the system. Paraphrase queries were written to avoid the memories' wording, but the set is small, synthetic and not independent. **It is not pilot evidence.** The numbers compare modes and models on this set. They do not predict quality on real team knowledge.
- **Metrics:** recall@5 = |relevant ∩ top 5| / |relevant|, averaged over queries. MRR = 1 / rank of the first relevant result in the top 10 (0 if none), averaged. Searches run from the query's scope, so only that scope and its ancestors are candidates, as in production.
- **Thresholds were tuned on the same queries they are reported on.** There is no held-out split, so the "with floor" numbers are optimistic.
- **Run-to-run variation:** the tables are from the final run (after the review fixes). An earlier run, before those fixes (none of which touched ranking), gave identical recall figures, but hybrid MRR was 0.02–0.06 lower (nomic raw 0.749, with floor 0.784). Equal-rank ties are broken by `updated_at` and random memory IDs, which differ on every load. Treat MRR differences below about 0.05 as noise.
- **Environment:** Ollama 0.18.0 on a separate machine over an HTTPS link, and PostgreSQL 17 / pgvector 0.8.1 in Docker on an Apple Silicon Mac. The latencies include query embedding over that network. They are informational, not a performance target.

## Results by mode

Raw mode comparison (no vector similarity floor):

| Mode | nomic-embed-text recall@5 | MRR | qwen3-embedding:0.6b recall@5 | MRR |
| --- | --- | --- | --- | --- |
| Full-text, stage 01 (all terms required) | 0.560 | 0.595 | (same) | (same) |
| Full-text, stage 02 (lexemes OR-ed) | 0.893 | 0.689 | (same) | (same) |
| Vector only | 0.810 | 0.496 | 0.988 | 0.618 |
| **Hybrid (RRF + boost + precedence)** | **0.917** | **0.773** | **0.929** | **0.775** |

Hybrid with each model's chosen similarity floor (the shipped default):

| Model | Floor | Hybrid recall@5 | MRR |
| --- | --- | --- | --- |
| nomic-embed-text | 0.60 | 0.917 | 0.795 |
| qwen3-embedding:0.6b | 0.50 | 0.964 | 0.905 |

By category (recall@5 / MRR, no floor):

| Category | Full-text | Vector (nomic) | Hybrid (nomic) | Vector (qwen) | Hybrid (qwen) |
| --- | --- | --- | --- | --- | --- |
| Identifier (9) | 1.000 / 0.889 | 0.778 / 0.550 | 1.000 / 0.889 | 1.000 / 0.569 | 1.000 / 0.870 |
| Acronym (11) | 0.864 / 0.909 | 0.636 / 0.447 | 0.864 / 0.939 | 1.000 / 0.727 | 0.955 / 0.909 |
| Repository (5) | 0.800 / 0.412 | 0.900 / 0.500 | 1.000 / 0.490 | 0.900 / 0.533 | 0.900 / 0.500 |
| Paraphrase (17) | 0.882 / 0.523 | 0.912 / 0.499 | 0.882 / 0.687 | 1.000 / 0.598 | 0.882 / 0.718 |

Median search latency (hybrid, end to end in-process): 65 ms with nomic and 145 ms with qwen. Almost all of it is the query embedding round trip. Full-text alone took 2 ms.

## Findings

1. **Hybrid beats either retriever alone with nomic-embed-text,** on both recall and MRR. Full-text is perfect on identifiers. Vector search is weak on identifiers and acronyms (`DB_POOL_MAX_CONNS`, `retryWithJitter`, `CQRS`, `PCI DSS`) and helps with repository names and paraphrases. That matches the proposal's concern about embedding models and code vocabulary.
2. **The biggest single gain was not vectors.** Stage 01's full-text search required every term: 0.059 recall on paraphrases, 0.560 overall. OR-ing the query lexemes raised full-text to 0.893 overall. Questions such as "how should I represent money amounts?" now match. Queries with explicit websearch syntax (quotes, `OR`, `-term`) keep strict semantics.
3. **qwen3-embedding:0.6b is clearly the better vector retriever here** (0.988 vs 0.810 recall@5, and it gets every acronym and identifier). With raw RRF, hybrid **lowers** its recall slightly (0.929 vs 0.988 vector-only). Weak full-text matches dilute its strong vector ranks. Its floor (0.50) recovers most of that, and with floors applied its hybrid score is 0.964 / 0.905, against nomic's 0.917 / 0.795.
4. **The ranking is still mediocre.** MRR stays well below recall. A relevant memory is usually in the top 5 but often not first. Repository-name queries have low MRR (≈0.5), because many memories mention the same repository.
5. **The similarity floor matters for unanswerable queries.** Without a floor, all five unanswerable queries return 10 results, 7 of them through the vector retriever alone. With nomic at 0.60 they return 3.0 on average, almost all keyword matches on words like "process" or "choice". With qwen at 0.50 they also return 3.0. The floors are above some relevant similarities: nomic's 10th percentile of relevant similarity is 0.544. They work because full-text covers what the vector side drops. They are tuned on this set and are starting points.

Hybrid misses with nomic and no floor (recall@5 is unchanged at the 0.60 floor): q11, q17 and q19 each found one of their two relevant memories. The project-level memory used none of the query's terms ("availability objective" for SLO, "pseudonymized identifier" for PII, "Jaeger client" for OTLP). q32 ("where should the iPhone app keep login credentials?") and q33 ("what tool turns raw tables into analytics models?") missed entirely.

## Model choice

**`nomic-embed-text` stays the default.** On this set, qwen3-embedding:0.6b is better on every vector metric. But the prompt's bar is that the eval *clearly* favours another model, and these results come from 42 synthetic queries that I wrote, with floors tuned on the same queries. qwen is also twice as slow per query embedding here (≈135 ms vs ≈65 ms), and its weights are 2.3× larger (639 MB vs 274 MB). **Recommendation:** run this harness on real pilot queries before stage 07, and switch if the gap holds. Switching is configuration only (`TEAM_MEMORY_EMBEDDING_MODEL=qwen3-embedding:0.6b`, plus `team-memory embeddings add-model`). See `docs/decisions/embedding-models.md`. The eval itself registered qwen's 1024-dimension space next to the 768-dimension default, with `embeddings.AddModel`, the function behind the CLI command.

## Conflict-candidate detector

The detector looks at same-kind memory pairs in ancestor/descendant scopes whose cosine similarity is at or above a threshold. It runs in the worker after each embedding and stores suggestions in `conflict_candidates`. It never changes search results. The set has 379 same-kind ancestor/descendant pairs and 8 labeled conflicts. One labeled conflict, `ana-region` (a decision to process data in us-east-1) against `org-pii` (a convention keeping personal data in the EU), crosses kinds. The detector cannot find it by design, so recall is at most 7/8 = 0.875.

| Threshold | nomic detected | precision | recall | qwen detected | precision | recall |
| --- | --- | --- | --- | --- | --- | --- |
| 0.65 | 142 | 0.049 | 0.875 | 11 | 0.636 | 0.875 |
| 0.70 | 29 | 0.241 | 0.875 | 7 | **0.857** | **0.750** |
| 0.75 | 6 | **0.833** | **0.625** | 4 | 0.750 | 0.375 |
| 0.80 | 4 | 1.000 | 0.500 | 0 | – | 0 |

The defaults are 0.75 for nomic and 0.70 for qwen (bold). **The precision values rest on 4 to 7 detections each, so they are anecdotes, not estimates.** At nomic 0.75, the false positive was `data-pii` against `org-pii`: a team convention that elaborates the organization's rule, at 0.773 similarity. Similarity finds *same-topic* pairs. Whether two memories *disagree* is a separate question that embeddings do not answer. Same-kind hard negatives that elaborate a parent memory sit at 0.66–0.77 with nomic, inside the band where true conflicts also sit (0.71–0.88). Treat candidates as review-queue suggestions only, as designed. In the final run, the worker detected candidates while embedding at the shipped thresholds: 6 pairs for nomic at 0.75 and 7 for qwen at 0.70. Both sets matched the offline computation exactly (`stored_matches_offline: true`), so the stored suggestions are what the table above describes.

## Limitations

- One small synthetic dataset, written by the implementer, with no held-out split.
- No judgments of graded relevance; a result counts only if it is labeled relevant.
- Two models only, both through Ollama. Long memories (truncated at the model context) are not represented.
- The latency figures mix network and model time on one machine. The search-latency target (p95 < 500 ms warmed, excluding query embedding) is assessed by the index benchmark (`docs/decisions/vector-index-strategy.md`), not here.
