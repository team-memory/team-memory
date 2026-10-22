-- Exceptional purge of one memory's text (docs/operations/secret-purge.md).
--
-- Outside application privileges: run it as the owner of the Team Memory
-- tables (the migration role), only with explicit operator authorization,
-- after the exposed credential has been rotated:
--
--   psql "$OPERATOR_URL" -v memory_id=<uuid> -v ticket='<authorization ref>' -f purge-memory.sql
--
-- What it does, in one transaction: replaces the title, content, rationale,
-- source URI and change notes of every revision of the memory (and review
-- notes and promotion snapshots that may quote it) with a marker, removes
-- its embeddings and conflict candidates, marks its mirror events purged,
-- archives it if active, and appends an audit event. Rows and ids stay, so
-- history, approvals and references remain consistent; immutability
-- triggers are disabled only inside this transaction and are re-enabled
-- before it commits.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL lock_timeout = '10s';
SELECT set_config('tm.purge_id', :'memory_id', true);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM memories WHERE id = current_setting('tm.purge_id')::uuid) THEN
        RAISE EXCEPTION 'memory % not found', current_setting('tm.purge_id');
    END IF;
END $$;

ALTER TABLE memory_revisions DISABLE TRIGGER memory_revisions_immutable;
ALTER TABLE approvals DISABLE TRIGGER approvals_immutable;
ALTER TABLE promotion_requests DISABLE TRIGGER promotion_requests_before_update;

UPDATE memory_revisions
SET title = '[purged]', content = '[purged by operator]', rationale = '', source_uri = NULL, change_note = '[purged]'
WHERE memory_id = current_setting('tm.purge_id')::uuid;

UPDATE approvals SET note = '[purged]'
WHERE memory_id = current_setting('tm.purge_id')::uuid AND note <> '';

UPDATE promotion_requests
SET snapshot = jsonb_set(jsonb_set(jsonb_set(snapshot, '{title}', '"[purged]"'), '{content}', '"[purged by operator]"'), '{rationale}', '""')
               - 'source_uri',
    rationale = '[purged]'
WHERE source_memory_id = current_setting('tm.purge_id')::uuid
   OR result_memory_id = current_setting('tm.purge_id')::uuid;

ALTER TABLE memory_revisions ENABLE TRIGGER memory_revisions_immutable;
ALTER TABLE approvals ENABLE TRIGGER approvals_immutable;
ALTER TABLE promotion_requests ENABLE TRIGGER promotion_requests_before_update;

DELETE FROM memory_embeddings WHERE memory_id = current_setting('tm.purge_id')::uuid;
DELETE FROM conflict_candidates
WHERE memory_id = current_setting('tm.purge_id')::uuid OR candidate_memory_id = current_setting('tm.purge_id')::uuid;
UPDATE jobs SET status = 'succeeded', last_error = 'purged', updated_at = now()
WHERE status IN ('pending', 'running', 'failed', 'dead')
  AND (payload->>'memory_id')::uuid = current_setting('tm.purge_id')::uuid;
UPDATE mirror_state SET delivery_state = 'purged', last_error = 'purged by operator', updated_at = now()
WHERE memory_id = current_setting('tm.purge_id')::uuid;
UPDATE jobs SET status = 'succeeded', last_error = 'purged', updated_at = now()
WHERE kind = 'git_mirror' AND status IN ('pending', 'running', 'failed', 'dead')
  AND (payload->>'event_id')::uuid IN (SELECT id FROM memory_events WHERE memory_id = current_setting('tm.purge_id')::uuid);

-- Out of default retrieval (no lifecycle event is invented: the audit
-- event below records what happened).
UPDATE memories SET status = 'archived', state_version = state_version + 1, updated_at = now()
WHERE id = current_setting('tm.purge_id')::uuid AND status = 'active';

INSERT INTO audit_events (org_id, actor_id, action, target_type, target_id, scope_id, outcome, details)
SELECT org_id, NULL, 'memory.purge', 'memory', id::text, scope_id, 'success',
       jsonb_build_object('via', 'operator-sql', 'authorization', :'ticket')
FROM memories WHERE id = current_setting('tm.purge_id')::uuid;

-- The immutability trigger is back on: an update must be refused
-- (insufficient_privilege); if it is not, abort the whole purge.
DO $$
BEGIN
    BEGIN
        UPDATE memory_revisions SET content = 'x' WHERE memory_id = current_setting('tm.purge_id')::uuid;
    EXCEPTION WHEN insufficient_privilege THEN
        RETURN;
    END;
    RAISE EXCEPTION 'immutability trigger not re-enabled; purge rolled back';
END $$;
COMMIT;
\echo purged; now handle the mirror, backups and copies (secret-purge.md)
