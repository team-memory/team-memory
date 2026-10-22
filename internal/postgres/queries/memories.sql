-- name: InsertMemory :one
INSERT INTO memories (org_id, scope_id, kind, trust, current_revision, created_by,
                      promoted_from_memory_id, promoted_from_revision)
VALUES (@org_id, @scope_id, @kind, @trust, 1, @created_by,
        sqlc.narg('promoted_from_memory_id'), sqlc.narg('promoted_from_revision'))
RETURNING *;

-- name: InsertRevision :one
INSERT INTO memory_revisions (memory_id, revision, org_id, scope_id, kind, title, content,
                              rationale, source_uri, changed_by, change_note)
VALUES (@memory_id, @revision, @org_id, @scope_id, @kind, @title, @content,
        @rationale, sqlc.narg('source_uri'), @changed_by, @change_note)
RETURNING memory_id, revision, created_at;

-- name: InsertRevisionOverride :exec
INSERT INTO revision_overrides (memory_id, revision, org_id, scope_id, overridden_memory_id)
VALUES ($1, $2, $3, $4, $5);

-- name: InsertMemoryEvent :one
INSERT INTO memory_events (org_id, scope_id, memory_id, revision, event_type, actor_id, snapshot)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, seq, created_at;

-- name: GetMemoryScope :one
SELECT scope_id FROM memories WHERE id = $1;

-- name: LockMemory :one
SELECT * FROM memories WHERE id = $1 FOR UPDATE;

-- name: SetMemoryRevision :one
UPDATE memories
SET current_revision = @revision, kind = @kind, trust = @trust,
    state_version = state_version + 1, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetMemorySuperseded :one
UPDATE memories
SET status = 'superseded', superseded_by = @superseded_by,
    state_version = state_version + 1, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetMemoryTrust :one
UPDATE memories
SET trust = @trust, state_version = state_version + 1, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetMemoryArchived :one
UPDATE memories
SET status = 'archived', state_version = state_version + 1, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: GetMemoryView :one
SELECT m.id, m.org_id, m.scope_id, s.path AS scope_path, m.kind, m.status, m.trust,
       m.current_revision, m.state_version, m.superseded_by,
       m.promoted_from_memory_id, m.promoted_from_revision,
       m.created_by, m.created_at, m.updated_at,
       r.title, r.content, r.rationale, r.source_uri, r.changed_by, r.created_at AS revised_at,
       ap.decision AS review_decision, ap.reviewer_id, ap.decided_at AS reviewed_at
FROM memories m
JOIN memory_revisions r ON r.memory_id = m.id AND r.revision = m.current_revision
JOIN scopes s ON s.id = m.scope_id
-- Latest review outcome of the current revision.
LEFT JOIN approvals ap ON ap.id = (
    SELECT a.id FROM approvals a
    WHERE a.memory_id = m.id AND a.revision = m.current_revision
    ORDER BY a.decided_at DESC, a.id DESC LIMIT 1)
WHERE m.id = $1;

-- name: ListRevisions :many
SELECT memory_id, revision, org_id, scope_id, kind, title, content, rationale,
       source_uri, changed_by, change_note, created_at
FROM memory_revisions
WHERE memory_id = $1
ORDER BY revision;

-- name: ListRevisionOverrides :many
SELECT revision, overridden_memory_id
FROM revision_overrides
WHERE memory_id = $1
ORDER BY revision, overridden_memory_id;

-- name: ListMemoryEvents :many
SELECT id, seq, revision, event_type, actor_id, snapshot, created_at
FROM memory_events
WHERE memory_id = $1
ORDER BY seq;

-- name: RecentMemories :many
SELECT m.id, m.scope_id, s.path AS scope_path, m.kind, m.status, m.trust,
       m.current_revision, m.created_by, m.updated_at,
       r.title, r.content, r.rationale, r.source_uri,
       ap.reviewer_id, ap.decided_at AS reviewed_at
FROM memories m
JOIN memory_revisions r ON r.memory_id = m.id AND r.revision = m.current_revision
JOIN scopes s ON s.id = m.scope_id
LEFT JOIN approvals ap ON m.trust = 'approved' AND ap.id = (
    SELECT a.id FROM approvals a
    WHERE a.memory_id = m.id AND a.revision = m.current_revision
    ORDER BY a.decided_at DESC, a.id DESC LIMIT 1)
WHERE m.scope_id = ANY(@scope_ids::uuid[])
  AND m.status = 'active'
  AND (cardinality(@kinds::text[]) = 0 OR m.kind = ANY(@kinds::text[]))
ORDER BY m.updated_at DESC, m.id
LIMIT @max_results;
