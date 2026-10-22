-- +goose Up
-- Stage 06: one-way git mirror. See docs/decisions/mirror-security.md and
-- docs/operations/mirror.md.
--
-- Exportable lifecycle events enqueue one git_mirror job and one
-- mirror_state row per (event, repository) from a trigger, so every path
-- that writes an event (Go services and the promotion definer function)
-- enqueues in the same transaction. Nothing is enqueued until an operator
-- enables the mirror (`team-memory mirror enable`), which also backfills
-- earlier events.
--
-- The export worker connects as a member of team_memory_mirror only: it
-- reads exactly the event, revision and display-name data the mirror
-- exports, and owns the git_mirror part of the job queue.

-- +goose StatementBegin
DO $$
BEGIN
    CREATE ROLE team_memory_mirror NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
EXCEPTION WHEN duplicate_object OR unique_violation THEN
    NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
    GRANT USAGE ON SCHEMA public TO team_memory_mirror;
EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'not the owner of schema public; grant USAGE to team_memory_mirror manually';
END;
$$;
-- +goose StatementEnd

-- held: not exported because an earlier event of the same memory failed
-- (secret_detected) or was purged; purged: removed by the purge runbook and
-- never re-exported.
ALTER TABLE mirror_state DROP CONSTRAINT mirror_state_delivery_state_check;
ALTER TABLE mirror_state ADD CONSTRAINT mirror_state_delivery_state_check
    CHECK (delivery_state IN ('pending', 'committed', 'pushed', 'failed', 'held', 'purged'));
ALTER TABLE mirror_state
    ADD COLUMN memory_id  uuid,
    ADD COLUMN event_seq  bigint,
    ADD COLUMN last_error text CHECK (last_error IS NULL OR length(last_error) <= 2000);
UPDATE mirror_state s SET memory_id = e.memory_id, event_seq = e.seq FROM memory_events e WHERE e.id = s.event_id;
ALTER TABLE mirror_state ALTER COLUMN memory_id SET NOT NULL, ALTER COLUMN event_seq SET NOT NULL;
CREATE INDEX mirror_state_repo_seq_idx ON mirror_state (repository, event_seq);
CREATE INDEX mirror_state_memory_idx ON mirror_state (memory_id, event_seq);

-- One row: whether exports are enqueued. Not tenant data.
CREATE TABLE mirror_settings (
    singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    enabled    boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO mirror_settings DEFAULT VALUES;

-- Exportable event types. Review requests, rejections (the note is reviewer
-- free text) and promotion submissions (consent to share with the parent's
-- reviewers, not with the mirror) are not exported.
-- +goose StatementBegin
CREATE FUNCTION tm_mirror_exportable(p_type text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT p_type IN ('created', 'revised', 'approved', 'archived', 'superseded', 'promotion_finalized')
$$;
-- +goose StatementEnd

-- Enqueue for one event. The repository key is the owning scope's immutable
-- id; the worker maps it to a remote URL.
-- +goose StatementBegin
CREATE FUNCTION tm_mirror_enqueue(p_event memory_events) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    repo text := p_event.scope_id::text;
BEGIN
    INSERT INTO mirror_state (event_id, repository, org_id, memory_id, event_seq)
    VALUES (p_event.id, repo, p_event.org_id, p_event.memory_id, p_event.seq)
    ON CONFLICT (event_id, repository) DO NOTHING;
    INSERT INTO jobs (org_id, kind, dedupe_key, payload)
    VALUES (p_event.org_id, 'git_mirror', 'git_mirror:' || repo || ':' || p_event.id::text,
            jsonb_build_object('event_id', p_event.id, 'repository', repo))
    ON CONFLICT (dedupe_key) DO NOTHING;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION tm_mirror_after_event() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF tm_mirror_exportable(NEW.event_type) AND (SELECT enabled FROM mirror_settings) THEN
        PERFORM tm_mirror_enqueue(NEW);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER memory_events_mirror AFTER INSERT ON memory_events
    FOR EACH ROW EXECUTE FUNCTION tm_mirror_after_event();

-- Definer functions are not callable by PUBLIC.
REVOKE ALL ON FUNCTION tm_mirror_enqueue(memory_events) FROM PUBLIC;
REVOKE ALL ON FUNCTION tm_mirror_after_event() FROM PUBLIC;
-- `team-memory mirror enable` backfills through it.
GRANT EXECUTE ON FUNCTION tm_mirror_enqueue(memory_events) TO team_memory_operator;

------------------------------------------------------------------------------
-- RLS and privileges for the export worker
------------------------------------------------------------------------------
-- The mirror is operator-only and exports every owning scope, so the export
-- role reads all rows of the tables it exports from; column grants keep
-- out what may never be exported (review notes, rejection notes,
-- identities). It cannot write tenant data.

CREATE POLICY mirror_select ON scopes FOR SELECT TO team_memory_mirror USING (true);
CREATE POLICY mirror_select ON memories FOR SELECT TO team_memory_mirror USING (true);
CREATE POLICY mirror_select ON memory_revisions FOR SELECT TO team_memory_mirror USING (true);
CREATE POLICY mirror_select ON revision_overrides FOR SELECT TO team_memory_mirror USING (true);
CREATE POLICY mirror_select ON memory_events FOR SELECT TO team_memory_mirror USING (true);
CREATE POLICY mirror_select ON approvals FOR SELECT TO team_memory_mirror USING (decision = 'approved');
CREATE POLICY mirror_all ON jobs TO team_memory_mirror
    USING (kind = 'git_mirror') WITH CHECK (kind = 'git_mirror');
CREATE POLICY mirror_all ON mirror_state TO team_memory_mirror USING (true) WITH CHECK (true);

GRANT SELECT (id, org_id, parent_id, path, level, slug, name) ON scopes TO team_memory_mirror;
GRANT SELECT (id, org_id, scope_id, status, promoted_from_memory_id, promoted_from_revision, created_by, created_at)
    ON memories TO team_memory_mirror;
GRANT SELECT (memory_id, revision, org_id, scope_id, kind, title, content, rationale, source_uri,
              changed_by, change_note, created_at) ON memory_revisions TO team_memory_mirror;
GRANT SELECT ON revision_overrides TO team_memory_mirror;
GRANT SELECT ON memory_events TO team_memory_mirror;
GRANT SELECT (memory_id, revision, event_id, decision, reviewer_id, decided_at) ON approvals TO team_memory_mirror;
GRANT SELECT (id, kind, display_name) ON principals TO team_memory_mirror;
GRANT SELECT, UPDATE ON jobs TO team_memory_mirror;
GRANT SELECT, UPDATE ON mirror_state TO team_memory_mirror;
GRANT SELECT ON mirror_settings TO team_memory_mirror;

GRANT SELECT, INSERT, UPDATE, DELETE ON mirror_settings TO team_memory_operator;

-- The embedding worker no longer touches mirror_state.
DROP POLICY worker_all ON mirror_state;
REVOKE ALL ON mirror_state FROM team_memory_worker;
-- And it claims only embedding jobs.
DROP POLICY worker_all ON jobs;
CREATE POLICY worker_all ON jobs TO team_memory_worker USING (kind = 'embed') WITH CHECK (kind = 'embed');

-- +goose Down
-- +goose StatementBegin
DROP POLICY worker_all ON jobs;
CREATE POLICY worker_all ON jobs TO team_memory_worker USING (true) WITH CHECK (true);
GRANT SELECT, INSERT, UPDATE ON mirror_state TO team_memory_worker;
CREATE POLICY worker_all ON mirror_state TO team_memory_worker USING (true) WITH CHECK (true);
DROP POLICY mirror_select ON scopes;
DROP POLICY mirror_select ON memories;
DROP POLICY mirror_select ON memory_revisions;
DROP POLICY mirror_select ON revision_overrides;
DROP POLICY mirror_select ON memory_events;
DROP POLICY mirror_select ON approvals;
DROP POLICY mirror_all ON jobs;
DROP POLICY mirror_all ON mirror_state;
DROP TRIGGER memory_events_mirror ON memory_events;
DROP FUNCTION tm_mirror_after_event();
DROP FUNCTION tm_mirror_enqueue(memory_events);
DROP FUNCTION tm_mirror_exportable(text);
DROP TABLE mirror_settings;
DROP INDEX mirror_state_memory_idx;
DROP INDEX mirror_state_repo_seq_idx;
ALTER TABLE mirror_state DROP COLUMN last_error, DROP COLUMN event_seq, DROP COLUMN memory_id;
ALTER TABLE mirror_state DROP CONSTRAINT mirror_state_delivery_state_check;
ALTER TABLE mirror_state ADD CONSTRAINT mirror_state_delivery_state_check
    CHECK (delivery_state IN ('pending', 'committed', 'pushed', 'failed'));
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM team_memory_mirror;
REVOKE USAGE ON SCHEMA public FROM team_memory_mirror;
-- +goose StatementEnd
