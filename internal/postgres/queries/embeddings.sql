-- name: GetEmbeddingModel :one
SELECT * FROM embedding_models WHERE name = $1;

-- name: ListEmbeddingModels :many
SELECT * FROM embedding_models ORDER BY created_at, name;

-- name: InsertEmbeddingModel :one
INSERT INTO embedding_models (name, dimensions, enabled)
VALUES (@name, @dimensions, @enabled)
RETURNING *;

-- name: SetEmbeddingModelEnabled :execrows
UPDATE embedding_models SET enabled = @enabled WHERE name = @name;

-- name: GetRevisionForEmbedding :one
SELECT r.memory_id, r.revision, r.org_id, r.scope_id, r.kind, r.title, r.content, r.rationale,
       m.current_revision, m.status
FROM memory_revisions r
JOIN memories m ON m.id = r.memory_id
WHERE r.memory_id = @memory_id AND r.revision = @revision;

-- name: EmbeddingExists :one
SELECT EXISTS (
    SELECT 1 FROM memory_embeddings
    WHERE memory_id = @memory_id AND revision = @revision AND model = @model
)::boolean;

-- name: LockMemoryRevision :one
-- Shares the memory row lock so a new revision cannot commit while the
-- embedding of the current one is being stored.
SELECT current_revision, status FROM memories WHERE id = @id FOR SHARE;

-- name: InsertEmbedding :execrows
INSERT INTO memory_embeddings (memory_id, revision, org_id, scope_id, model, dimensions, embedding)
VALUES (@memory_id, @revision, @org_id, @scope_id, @model, @dimensions, @embedding)
ON CONFLICT (memory_id, revision, model) DO NOTHING;

-- name: DeleteOlderEmbeddings :execrows
-- Embeddings are derived data; only the current revision's is searched, so
-- older ones are dropped once a newer one exists.
DELETE FROM memory_embeddings
WHERE memory_id = @memory_id AND model = @model AND revision < @revision;

-- name: InsertConflictCandidate :exec
INSERT INTO conflict_candidates (org_id, scope_id, memory_id, revision, candidate_scope_id,
                                 candidate_memory_id, candidate_revision, model, similarity)
VALUES (@org_id, @scope_id, @memory_id, @revision, @candidate_scope_id,
        @candidate_memory_id, @candidate_revision, @model, @similarity)
ON CONFLICT DO NOTHING;

-- name: ListConflictCandidates :many
SELECT * FROM conflict_candidates
WHERE model = @model AND status = 'open'
ORDER BY similarity DESC, memory_id, candidate_memory_id;

-- name: RelatedScopes :many
-- Strict ancestors and strict descendants of a scope: the scopes whose
-- memories can meet this scope's memories in some reader's results.
SELECT up.ancestor_id AS scope_id FROM scope_closure up WHERE up.descendant_id = @scope_id AND up.distance > 0
UNION
SELECT down.descendant_id FROM scope_closure down WHERE down.ancestor_id = @scope_id AND down.distance > 0;
