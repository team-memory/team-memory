-- name: InsertApproval :one
INSERT INTO approvals (org_id, scope_id, memory_id, revision, event_id, decision, reviewer_id, note)
VALUES (@org_id, @scope_id, @memory_id, @revision, @event_id, @decision, @reviewer_id, @note)
RETURNING *;

-- name: LatestReviewOutcome :one
SELECT * FROM approvals
WHERE memory_id = @memory_id AND revision = @revision
ORDER BY decided_at DESC, id DESC
LIMIT 1;

-- name: InsertReviewRequest :execrows
-- memory_approve: one review request per exact revision; repeats are no-ops.
INSERT INTO memory_events (org_id, scope_id, memory_id, revision, event_type, actor_id, snapshot)
VALUES (@org_id, @scope_id, @memory_id, @revision, 'review_requested', @actor_id, @snapshot)
ON CONFLICT (memory_id, revision) WHERE event_type = 'review_requested' DO NOTHING;

-- name: ReviewQueue :many
-- Active memories in the given scopes whose current revision has no review
-- outcome yet and is not approved. Writers' proposals enter the queue on
-- creation or update; requested marks an explicit memory_approve request.
SELECT m.id, m.scope_id, s.path AS scope_path, m.kind, m.trust, m.current_revision, m.state_version,
       m.created_by, m.updated_at, r.title, r.changed_by,
       EXISTS (SELECT 1 FROM memory_events e WHERE e.memory_id = m.id AND e.revision = m.current_revision
                 AND e.event_type = 'review_requested')::boolean AS requested,
       EXISTS (SELECT 1 FROM revision_overrides o WHERE o.memory_id = m.id AND o.revision = m.current_revision)::boolean AS proposes_override
FROM memories m
JOIN memory_revisions r ON r.memory_id = m.id AND r.revision = m.current_revision
JOIN scopes s ON s.id = m.scope_id
WHERE m.scope_id = ANY(@scope_ids::uuid[])
  AND m.status = 'active' AND m.trust <> 'approved'
  AND NOT EXISTS (SELECT 1 FROM approvals a WHERE a.memory_id = m.id AND a.revision = m.current_revision)
ORDER BY requested DESC, m.updated_at, m.id
LIMIT @max_results;

-- name: InsertPromotionRequest :one
INSERT INTO promotion_requests (org_id, source_memory_id, source_revision, source_scope_id,
                                destination_scope_id, submitted_by, rationale, snapshot)
VALUES (@org_id, @source_memory_id, @source_revision, @source_scope_id,
        @destination_scope_id, @submitted_by, @rationale, @snapshot)
RETURNING *;

-- name: GetLivePromotionRequest :one
SELECT * FROM promotion_requests
WHERE source_memory_id = @source_memory_id AND source_revision = @source_revision
  AND status IN ('pending', 'accepted');

-- name: GetPromotionRequest :one
SELECT * FROM promotion_requests WHERE id = $1;

-- name: LockPromotionRequest :one
SELECT * FROM promotion_requests WHERE id = $1 FOR UPDATE;

-- name: DecidePromotionRequest :one
UPDATE promotion_requests
SET status = @status, decided_by = @decided_by, decided_at = now()
WHERE id = @id AND status = 'pending'
RETURNING *;

-- name: ListPromotionRequestsForDestinations :many
SELECT * FROM promotion_requests
WHERE destination_scope_id = ANY(@scope_ids::uuid[]) AND status = 'pending'
ORDER BY created_at, id;

-- name: FinalizePromotionSource :exec
SELECT tm_promotion_finalize_source(@request_id::uuid, @result_memory_id::uuid);

-- name: InsertPromotedMemory :one
INSERT INTO memories (org_id, scope_id, kind, trust, current_revision, created_by,
                      promoted_from_memory_id, promoted_from_revision)
VALUES (@org_id, @scope_id, @kind, @trust, 1, @created_by, @promoted_from_memory_id, @promoted_from_revision)
RETURNING *;

-- name: ListAuditEvents :many
SELECT * FROM audit_events
WHERE scope_id = ANY(@scope_ids::uuid[])
ORDER BY occurred_at DESC, id DESC
LIMIT @max_results;
