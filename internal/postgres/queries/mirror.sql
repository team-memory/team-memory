-- Git mirror (stage 06). The export worker runs these as a member of
-- team_memory_mirror; verify and the operator commands as the operator.

-- name: MirrorUndelivered :many
-- Events of one repository not yet acknowledged as pushed, in event order.
SELECT event_id, event_seq, memory_id, delivery_state, commit_sha
FROM mirror_state
WHERE repository = @repository AND delivery_state IN ('pending', 'committed')
ORDER BY event_seq;

-- name: MirrorMemoryBlocked :one
-- True when an earlier event of the memory was not exported (secret found,
-- held or purged): later events are held so the file never skips ahead of
-- a removed revision.
SELECT EXISTS (
    SELECT 1 FROM mirror_state
    WHERE memory_id = @memory_id AND repository = @repository AND event_seq < @event_seq
      AND delivery_state IN ('failed', 'held', 'purged'))::boolean AS blocked;

-- name: MirrorEventExport :one
-- Everything an export needs, from immutable rows only: the event and its
-- snapshot, the bound revision, and display names.
SELECT e.id, e.seq, e.event_type, e.snapshot, e.created_at AS event_at, e.actor_id,
       actor.display_name AS actor_name,
       e.memory_id, e.revision, e.scope_id, s.path AS scope_path,
       r.kind, r.title, r.content, r.rationale, r.source_uri, r.change_note,
       r.changed_by, reviser.display_name AS changed_by_name, r.created_at AS revised_at,
       m.created_by, creator.display_name AS created_by_name, m.created_at AS memory_created_at,
       (m.promoted_from_memory_id IS NOT NULL)::boolean AS promoted
FROM memory_events e
JOIN memory_revisions r ON r.memory_id = e.memory_id AND r.revision = e.revision
JOIN memories m ON m.id = e.memory_id
JOIN scopes s ON s.id = e.scope_id
JOIN principals actor ON actor.id = e.actor_id
JOIN principals reviser ON reviser.id = r.changed_by
JOIN principals creator ON creator.id = m.created_by
WHERE e.id = @event_id;

-- name: MirrorApprovalAsOf :many
-- The approval of the bound revision that existed at the event, if any.
SELECT ap.reviewer_id, p.display_name AS reviewer_name, ap.decided_at
FROM approvals ap
JOIN memory_events ae ON ae.id = ap.event_id
JOIN principals p ON p.id = ap.reviewer_id
WHERE ap.memory_id = @memory_id AND ap.revision = @revision AND ap.decision = 'approved'
  AND ae.seq <= @seq
ORDER BY ae.seq DESC
LIMIT 1;

-- name: MirrorRevisionOverrides :many
SELECT overridden_memory_id
FROM revision_overrides
WHERE memory_id = @memory_id AND revision = @revision
ORDER BY overridden_memory_id;

-- name: MirrorMarkState :exec
UPDATE mirror_state
SET delivery_state = @delivery_state::text,
    commit_sha = coalesce(sqlc.narg(commit_sha)::text, commit_sha),
    last_error = sqlc.narg(last_error)::text,
    exported_at = CASE WHEN @delivery_state::text = 'pushed' THEN now() ELSE exported_at END,
    updated_at = now()
WHERE event_id = @event_id AND repository = @repository;

-- name: MirrorCountAttempt :exec
UPDATE mirror_state
SET attempts = attempts + 1, updated_at = now()
WHERE repository = @repository AND delivery_state IN ('pending', 'committed');

-- name: MirrorGetState :one
SELECT event_id, repository, delivery_state, commit_sha, last_error
FROM mirror_state
WHERE event_id = @event_id AND repository = @repository;

-- name: MirrorEnabled :one
SELECT enabled FROM mirror_settings;

-- name: MirrorSetEnabled :exec
UPDATE mirror_settings SET enabled = @enabled, updated_at = now();

-- name: MirrorBackfill :execrows
-- Enqueues every exportable event that has no mirror state yet, in order.
SELECT tm_mirror_enqueue(e)
FROM memory_events e
WHERE tm_mirror_exportable(e.event_type)
  AND NOT EXISTS (SELECT 1 FROM mirror_state s WHERE s.event_id = e.id)
ORDER BY e.seq;

-- name: MirrorStatus :many
SELECT repository, delivery_state, count(*)::integer AS events, max(updated_at)::timestamptz AS last_update
FROM mirror_state
GROUP BY repository, delivery_state
ORDER BY repository, delivery_state;

-- name: MirrorRepositories :many
-- Every scope that owns at least one memory with an exportable event.
SELECT DISTINCT e.scope_id, s.path
FROM memory_events e JOIN scopes s ON s.id = e.scope_id
WHERE tm_mirror_exportable(e.event_type)
ORDER BY s.path;

-- name: MirrorExpectedEvents :many
-- Per memory of one repository, its latest exportable event and the state
-- of every exportable event that has not been pushed.
SELECT DISTINCT ON (e.memory_id) e.memory_id, e.id AS event_id, e.seq,
       coalesce(ms.delivery_state, 'not_enqueued')::text AS delivery_state
FROM memory_events e
LEFT JOIN mirror_state ms ON ms.event_id = e.id AND ms.repository = @repository::text
WHERE e.scope_id = @scope_id AND tm_mirror_exportable(e.event_type)
ORDER BY e.memory_id, e.seq DESC;

-- name: MirrorLatestPushed :many
-- Per memory of one repository, the latest event acknowledged as pushed.
SELECT DISTINCT ON (ms.memory_id) ms.memory_id, ms.event_id, ms.event_seq, ms.commit_sha
FROM mirror_state ms
WHERE ms.repository = @repository AND ms.delivery_state = 'pushed'
ORDER BY ms.memory_id, ms.event_seq DESC;

-- name: MirrorUndeliveredSummary :many
SELECT ms.memory_id, ms.event_id, ms.event_seq, ms.delivery_state, ms.last_error
FROM mirror_state ms
WHERE ms.repository = @repository AND ms.delivery_state <> 'pushed'
ORDER BY ms.event_seq;

-- name: MirrorRequeueHeld :execrows
-- After an operator resolved the cause (purge or rule fix), held and
-- failed events of a repository go back to pending with fresh jobs.
WITH reset AS (
    UPDATE mirror_state SET delivery_state = 'pending', last_error = NULL, updated_at = now()
    WHERE repository = @repository AND delivery_state IN ('held', 'failed')
    RETURNING event_id, repository, org_id)
INSERT INTO jobs (org_id, kind, dedupe_key, payload)
SELECT org_id, 'git_mirror', 'git_mirror:' || repository || ':' || event_id::text || ':' || extract(epoch FROM now())::bigint::text,
       jsonb_build_object('event_id', event_id, 'repository', repository)
FROM reset
ON CONFLICT (dedupe_key) DO NOTHING;

-- name: MirrorMarkPurged :execrows
-- Purge runbook: these events are never exported again.
UPDATE mirror_state SET delivery_state = 'purged', last_error = 'purged by operator', updated_at = now()
WHERE memory_id = @memory_id;

-- name: MirrorUndeliveredCounts :many
SELECT delivery_state, count(*)::integer AS events
FROM mirror_state
WHERE delivery_state <> 'pushed'
GROUP BY delivery_state;
