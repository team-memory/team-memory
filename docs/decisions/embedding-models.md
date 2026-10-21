# Decision: embedding models and vector storage

Status: accepted (stage 02, 2026-10-21)

## Decision

Embedding spaces are registered in an `embedding_models` table (name, dimensions, enabled). `memory_embeddings.embedding` is an **untyped `vector` column**, and each model gets its **own partial HNSW index** on a dimension cast:

```sql
CREATE INDEX memory_embeddings_nomic_embed_text_a4134b0e_hnsw ON memory_embeddings
    USING hnsw ((embedding::vector(768)) vector_cosine_ops)
    WHERE model = 'nomic-embed-text';
```

Index names combine a sanitized prefix of the model name with a hash of the full name (`Model.IndexName`), so distinct models never collide after sanitizing or PostgreSQL's 63-byte truncation. Rows are stored per `(memory_id, revision, model)`. Knowledge records (`memories`, `memory_revisions`) never hold vectors, so changing models never rewrites them.

## How incompatible vectors are kept apart

- **In the schema.** A composite foreign key `(model, dimensions) → embedding_models (name, dimensions)` and `CHECK (vector_dims(embedding) = dimensions)` mean a row can only hold a vector of its model's registered size. A trigger forbids changing a registered model's name or dimensions. A model with different dimensions is a different model, with its own name and index.
- **In every query.** Search and conflict detection always filter `model = '<name>'`, cast to that model's `vector(N)`, and compare only with a query embedded by the same model. The model name is inlined as a literal so PostgreSQL can match the partial index. A bind parameter would not match in a generic plan. Names are restricted to `[a-z0-9._:/-]` (registry CHECK and `Model.Validate`), so inlining is safe.
- **At startup.** `serve` and `worker` refuse to start when the configured model is not registered, is disabled, or has different dimensions (`embeddings.CheckRegistered`). The worker also probes the provider. A response with the wrong length is a configuration error: the job is dead-lettered without retries (`ErrDimensionMismatch`).

## Introducing a second model

1. `team-memory embeddings add-model --name <model> [--dimensions N]` registers the model and builds its index with `CREATE INDEX CONCURRENTLY`. It then checks that the index is valid, because a failed concurrent build leaves an invalid index that `IF NOT EXISTS` would silently skip. This is an operator command, because DDL needs owner privileges and the application role from stage 04 will not have them. Known models (`nomic-embed-text`, `qwen3-embedding:0.6b`) carry their dimensions and task prefixes.
2. From then on, every new revision enqueues a job for each enabled model in the same transaction (the outbox).
3. Run a worker with `TEAM_MEMORY_EMBEDDING_MODEL=<model>`. At startup it backfills jobs for current revisions that have no embedding for its model, and it only claims its own model's jobs. Job keys are `embed:<model>|<memory>:<revision>`, and `|` cannot occur in a model name, so `nomic-embed-text` never claims `nomic-embed-text:v1.5` jobs (tested in `TestModelsClaimOnlyTheirOwnJobs`).
4. Switch `serve` to the new model once `jobs status` shows the backfill done. Each serve process queries exactly one model. Until the switch, the old model keeps serving.
5. Retire the old model with `team-memory embeddings disable <model>`, which stops new jobs and backfill for it. Do this only after `serve` has switched, because `serve` and `worker` refuse a disabled model. Its rows and index can be dropped by an operator later. That is not automated in v1.

The stage 02 evaluation registered `qwen3-embedding:0.6b` (1024 dimensions) next to the 768-dimension default with `embeddings.AddModel`, the function behind the CLI command (without `CONCURRENTLY`, on a fresh database). It then embedded and searched through the per-model index, and no knowledge record was touched. The CLI command itself was exercised only by `go build`, not run against a database.

## Alternatives considered

- **One table per model, or a typed column per dimension.** This gives the same isolation, but adds DDL, sqlc code and query variants for every model. The partial expression index gives per-model indexes on one table.
- **Fixed `vector(768)` (stage 01).** This works for one model only. Changing it means rewriting every row, or the dimension silently constrains model choice.
- **`halfvec` storage.** It halves the size and allows HNSW up to 4000 dimensions. Unnecessary at the measured sizes (see `vector-index-strategy.md`), and it is an independent later change per model.

## Consequences and limits

- Only the current revision's embedding is kept. When revision N is embedded, older revisions' vectors for that model are deleted, because they are derived data. A job for a revision that is no longer current succeeds without work. That is checked before the provider call and again under a share lock on the memory row before storing, so a revision committed while a request is in flight is never overwritten by the older vector.
- A disabled or unserved model's pending jobs stay pending. `jobs status` shows them.
- Ollama truncates inputs longer than the model's context (`truncate: true`). Very long memories are embedded from their beginning only, and full-text search still covers the whole text.
- Task prefixes matter. `nomic-embed-text` expects `search_document: ` and `search_query: `. `qwen3-embedding` expects an instruction on queries only. They are part of the model defaults and can be overridden by env vars. Changing a document prefix requires re-embedding.
