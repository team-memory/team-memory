-- name: FullTextCandidates :many
-- Full-text candidates over the current revision of memories in the given
-- scopes (the caller passes only the searcher's scope chain, so scope
-- filtering happens before ranking). Without explicit websearch operators the
-- query's lexemes are OR-ed, so natural-language questions match documents
-- that share some terms; ts_rank_cd still favours documents matching more.
SELECT m.id, ts_rank_cd(r.search, q.query)::real AS rank
FROM memories m
JOIN memory_revisions r ON r.memory_id = m.id AND r.revision = m.current_revision
CROSS JOIN (
    SELECT CASE WHEN @websearch::boolean
                THEN websearch_to_tsquery('english', @query::text)
                -- Each lexeme is quoted with tsquery's own rules (quotes and
                -- backslashes doubled), so no input can inject operators.
                ELSE (SELECT coalesce(string_agg('''' || replace(replace(t.lexeme, '\', '\\'), '''', '''''') || '''', ' | '), '')::tsquery
                      FROM unnest(to_tsvector('english', @query::text)) AS t)
           END AS query
) AS q
WHERE m.scope_id = ANY(@scope_ids::uuid[])
  AND m.status = ANY(@statuses::text[])
  AND (cardinality(@kinds::text[]) = 0 OR m.kind = ANY(@kinds::text[]))
  AND NOT (m.id = ANY(@exclude_ids::uuid[]))
  AND r.search @@ q.query
ORDER BY rank DESC, m.updated_at DESC, m.id
LIMIT @max_results::integer;

-- name: SearchRows :many
SELECT m.id, m.scope_id, s.path AS scope_path, s.depth AS scope_depth, m.kind, m.status, m.trust,
       m.current_revision, m.created_by, m.updated_at,
       r.title, r.content, r.rationale, r.source_uri,
       ap.reviewer_id, ap.decided_at AS reviewed_at
FROM memories m
JOIN memory_revisions r ON r.memory_id = m.id AND r.revision = m.current_revision
JOIN scopes s ON s.id = m.scope_id
-- The approval behind trust=approved: latest outcome of the current revision.
LEFT JOIN approvals ap ON m.trust = 'approved' AND ap.id = (
    SELECT a.id FROM approvals a
    WHERE a.memory_id = m.id AND a.revision = m.current_revision
    ORDER BY a.decided_at DESC, a.id DESC LIMIT 1)
WHERE m.id = ANY(@ids::uuid[]);

-- name: EffectiveOverrides :many
-- Overrides in effect for a reader of the given scope chain: the overriding
-- memory is active, owned by a scope in the chain, strictly below the
-- overridden memory's scope, and the latest review outcome of its current
-- revision (which carries the link) is an approval. Draft (unapproved) links
-- never appear here. Independent of the search query.
SELECT o.overridden_memory_id, o.memory_id AS overriding_memory_id
FROM revision_overrides o
JOIN memories m ON m.id = o.memory_id AND m.current_revision = o.revision
JOIN memories t ON t.id = o.overridden_memory_id
JOIN scope_closure c ON c.ancestor_id = t.scope_id AND c.descendant_id = m.scope_id AND c.distance > 0
WHERE m.scope_id = ANY(@scope_ids::uuid[])
  AND m.status = 'active'
  AND m.trust = 'approved'
  AND (SELECT a.decision FROM approvals a
       WHERE a.memory_id = m.id AND a.revision = m.current_revision
       ORDER BY a.decided_at DESC, a.id DESC
       LIMIT 1) = 'approved'
ORDER BY o.overridden_memory_id, o.memory_id;

-- name: CountMissingEmbeddings :one
-- Searchable memories whose current revision has no embedding for the model
-- yet (job pending, retrying or dead). They are reachable by full-text only.
SELECT count(*)::integer
FROM memories m
WHERE m.scope_id = ANY(@scope_ids::uuid[])
  AND m.status = ANY(@statuses::text[])
  AND NOT EXISTS (
      SELECT 1 FROM memory_embeddings e
      WHERE e.memory_id = m.id AND e.revision = m.current_revision AND e.model = @model::text);
