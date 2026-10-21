-- +goose Up
-- Stage 02: embedding model registry, per-model vector storage and indexes,
-- worker support on the jobs outbox, and conflict-candidate suggestions.
-- See docs/decisions/embedding-models.md and docs/decisions/vector-index-strategy.md.

-- One row per embedding space. Vectors from different models are never
-- compared: every query and index is restricted to a single model.
CREATE TABLE embedding_models (
    name       text PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9._:/-]{0,99}$'),
    -- HNSW on the vector type supports at most 2000 dimensions.
    dimensions integer NOT NULL CHECK (dimensions BETWEEN 1 AND 2000),
    -- Embedding jobs are enqueued for every enabled model.
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, dimensions)
);

-- A model's dimensions are part of its identity; a model with different
-- dimensions is a different model with its own name and index.
-- +goose StatementBegin
CREATE FUNCTION tm_embedding_models_before_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.name <> OLD.name OR NEW.dimensions <> OLD.dimensions THEN
        RAISE EXCEPTION 'embedding model name and dimensions cannot change; register a new model'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER embedding_models_before_update BEFORE UPDATE ON embedding_models
    FOR EACH ROW EXECUTE FUNCTION tm_embedding_models_before_update();

-- Default model. Its index is created below; `team-memory embeddings add-model`
-- registers further models and builds their indexes.
INSERT INTO embedding_models (name, dimensions) VALUES ('nomic-embed-text', 768);

-- The column becomes dimension-agnostic; the composite FK and CHECK keep each
-- row consistent with its registered model. The table is empty before stage 02.
ALTER TABLE memory_embeddings ALTER COLUMN embedding TYPE vector;
ALTER TABLE memory_embeddings
    ADD CONSTRAINT memory_embeddings_model_fk
        FOREIGN KEY (model, dimensions) REFERENCES embedding_models (name, dimensions),
    ADD CONSTRAINT memory_embeddings_dims_check
        CHECK (vector_dims(embedding) = dimensions);

-- Exact (filtered) scans by scope, and the per-model approximate index.
CREATE INDEX memory_embeddings_model_scope_idx ON memory_embeddings (model, scope_id);
-- Index names carry a hash of the model name (embeddings.Model.IndexName), so
-- distinct models never collide after sanitizing or 63-byte truncation.
CREATE INDEX memory_embeddings_nomic_embed_text_a4134b0e_hnsw ON memory_embeddings
    USING hnsw ((embedding::vector(768)) vector_cosine_ops)
    WHERE model = 'nomic-embed-text';

-- Worker support. lease_id identifies one claim: a worker may record an
-- outcome only while its claim is current, so a late result from an expired
-- lease (even from a worker with the same name) is ignored.
ALTER TABLE jobs ADD COLUMN lease_id uuid;
CREATE INDEX jobs_running_idx ON jobs (locked_at) WHERE status = 'running';
CREATE INDEX jobs_dead_idx ON jobs (kind, updated_at) WHERE status = 'dead';

-- Conflict candidates: pairs of same-kind memories in an ancestor/descendant
-- relationship whose embeddings are very similar. Suggestions only: they never
-- change search results. Stage 05's review queue consumes them.
-- memory_id is always the memory in the deeper (more specific) scope.
CREATE TABLE conflict_candidates (
    org_id              uuid NOT NULL,
    scope_id            uuid NOT NULL,
    memory_id           uuid NOT NULL,
    revision            integer NOT NULL,
    candidate_scope_id  uuid NOT NULL,
    candidate_memory_id uuid NOT NULL,
    candidate_revision  integer NOT NULL,
    model               text NOT NULL REFERENCES embedding_models (name),
    similarity          real NOT NULL CHECK (similarity BETWEEN -1 AND 1),
    status              text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'dismissed', 'linked')),
    detected_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, revision, candidate_memory_id, candidate_revision, model),
    FOREIGN KEY (memory_id, revision) REFERENCES memory_revisions (memory_id, revision),
    FOREIGN KEY (candidate_memory_id, candidate_revision) REFERENCES memory_revisions (memory_id, revision),
    FOREIGN KEY (scope_id, org_id) REFERENCES scopes (id, org_id),
    FOREIGN KEY (candidate_scope_id, org_id) REFERENCES scopes (id, org_id),
    CHECK (memory_id <> candidate_memory_id)
);
CREATE INDEX conflict_candidates_scope_idx ON conflict_candidates (scope_id, status);
CREATE INDEX conflict_candidates_candidate_scope_idx ON conflict_candidates (candidate_scope_id, status);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'migration 00002_retrieval cannot be reversed'; END $$;
-- +goose StatementEnd
