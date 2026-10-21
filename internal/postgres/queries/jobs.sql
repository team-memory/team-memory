-- name: EnqueueEmbedJobs :exec
-- Outbox write, in the same transaction as the revision: one embed job per
-- enabled model. The dedupe key makes enqueueing idempotent per
-- (model, memory, revision). Its format is embed:<model>|<memory>:<revision>;
-- '|' cannot occur in model names, so one model's key prefix never matches
-- another's (embeddings.KeyPrefix).
INSERT INTO jobs (org_id, kind, dedupe_key, payload)
SELECT @org_id::uuid, 'embed',
       'embed:' || em.name || '|' || CAST(@memory_id::uuid AS text) || ':' || CAST(@revision::integer AS text),
       jsonb_build_object('memory_id', @memory_id::uuid, 'revision', @revision::integer, 'model', em.name)
FROM embedding_models em
WHERE em.enabled
ON CONFLICT (dedupe_key) DO NOTHING;

-- name: BackfillEmbedJobs :execrows
-- Enqueues jobs for current revisions (active or superseded) that have no
-- embedding for the model and no job yet, if the model is enabled. Dead jobs
-- are left alone; use RetryDeadJobs to revive them.
INSERT INTO jobs (org_id, kind, dedupe_key, payload)
SELECT m.org_id, 'embed',
       'embed:' || @model::text || '|' || m.id::text || ':' || m.current_revision::text,
       jsonb_build_object('memory_id', m.id, 'revision', m.current_revision, 'model', @model::text)
FROM memories m
WHERE m.status IN ('active', 'superseded')
  AND EXISTS (SELECT 1 FROM embedding_models em WHERE em.name = @model::text AND em.enabled)
  AND NOT EXISTS (
      SELECT 1 FROM memory_embeddings e
      WHERE e.memory_id = m.id AND e.revision = m.current_revision AND e.model = @model::text)
ON CONFLICT (dedupe_key) DO NOTHING;

-- name: ClaimJobs :many
-- Claims ready jobs with a lease. SKIP LOCKED lets several workers poll the
-- same table without blocking each other. A claim counts as an attempt, so a
-- worker that crashes mid-job still moves the job toward dead-lettering.
-- key_prefix narrows the claim to one partition of a kind (for embed jobs,
-- one model: 'embed:<model>|'); an empty prefix claims any job of the kind.
-- Each claim gets a fresh lease_id that the outcome queries must present.
UPDATE jobs
SET status = 'running', attempts = attempts + 1, locked_at = now(),
    locked_by = @worker::text, lease_id = gen_random_uuid(), updated_at = now()
WHERE id IN (
    SELECT j.id FROM jobs j
    WHERE j.kind = @kind::text AND j.status = 'pending' AND j.run_after <= now()
      AND starts_with(coalesce(j.dedupe_key, ''), @key_prefix::text)
    ORDER BY j.run_after, j.id
    LIMIT @max_jobs::integer
    FOR UPDATE SKIP LOCKED)
RETURNING id, org_id, kind, payload, attempts, max_attempts, lease_id;

-- name: CompleteJob :execrows
UPDATE jobs
SET status = 'succeeded', locked_at = NULL, lease_id = NULL, last_error = NULL, updated_at = now()
WHERE id = @id AND status = 'running' AND lease_id = @lease_id::uuid;

-- name: RetryJob :execrows
UPDATE jobs
SET status = 'pending', run_after = now() + make_interval(secs => @delay_seconds::float8),
    locked_at = NULL, locked_by = NULL, lease_id = NULL, last_error = @last_error::text, updated_at = now()
WHERE id = @id AND status = 'running' AND lease_id = @lease_id::uuid;

-- name: DeadLetterJob :execrows
UPDATE jobs
SET status = 'dead', locked_at = NULL, locked_by = NULL, lease_id = NULL, last_error = @last_error::text, updated_at = now()
WHERE id = @id AND status = 'running' AND lease_id = @lease_id::uuid;

-- name: ReapExpiredJobs :execrows
-- Releases jobs whose worker lease expired (crash or partition). Jobs that
-- already used their attempts are dead-lettered.
UPDATE jobs
SET status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
    locked_at = NULL, locked_by = NULL, lease_id = NULL, run_after = now(),
    last_error = 'lease expired', updated_at = now()
WHERE status = 'running' AND locked_at < now() - make_interval(secs => @lease_seconds::float8);

-- name: RetryDeadJobs :execrows
UPDATE jobs
SET status = 'pending', attempts = 0, run_after = now(), updated_at = now()
WHERE status = 'dead' AND kind = @kind::text;

-- name: JobCounts :many
SELECT kind, status, count(*)::integer AS jobs
FROM jobs
GROUP BY kind, status
ORDER BY kind, status;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = $1;
