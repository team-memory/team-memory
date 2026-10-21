-- +goose Up
-- Foundation schema. See docs/decisions/scope-tree.md and docs/handoff/01-foundation.md
-- for how this differs from the draft schema in the proposal.

CREATE EXTENSION IF NOT EXISTS vector;

-- Shared trigger function: rejects UPDATE/DELETE/TRUNCATE on append-only tables.
-- +goose StatementBegin
CREATE FUNCTION tm_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only (% rejected)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

------------------------------------------------------------------------------
-- Principals and identities
------------------------------------------------------------------------------

CREATE TABLE principals (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL CHECK (kind IN ('human', 'service')),
    display_name text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    created_at   timestamptz NOT NULL DEFAULT now(),
    disabled_at  timestamptz
);

-- External identities (OIDC issuer + subject). Populated from stage 03/04; a
-- principal may have several once explicit SSO linking exists.
CREATE TABLE principal_identities (
    principal_id uuid NOT NULL REFERENCES principals (id),
    issuer       text NOT NULL,
    subject      text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX principal_identities_principal_idx ON principal_identities (principal_id);

CREATE TABLE personal_access_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id uuid NOT NULL REFERENCES principals (id),
    name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    token_prefix text NOT NULL,
    token_hash   bytea NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    last_used_at timestamptz
);
CREATE INDEX personal_access_tokens_principal_idx ON personal_access_tokens (principal_id);

------------------------------------------------------------------------------
-- Scope tree
------------------------------------------------------------------------------

CREATE TABLE scopes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL,
    parent_id  uuid,
    level      text NOT NULL CHECK (level ~ '^[a-z][a-z0-9_-]{0,39}$'),
    slug       text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    depth      integer NOT NULL CHECK (depth >= 0),
    path       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid REFERENCES principals (id),
    UNIQUE (id, org_id),
    FOREIGN KEY (parent_id, org_id) REFERENCES scopes (id, org_id),
    -- Exactly one root per organization, and the root is its own org.
    CHECK ((parent_id IS NULL) = (depth = 0)),
    CHECK ((parent_id IS NULL) = (id = org_id))
);
CREATE UNIQUE INDEX scopes_parent_slug_idx ON scopes (parent_id, slug) WHERE parent_id IS NOT NULL;
CREATE INDEX scopes_org_idx ON scopes (org_id);

-- Closure table: one row per (ancestor, descendant) pair, including self (distance 0).
-- Scopes cannot move in v1, so rows are written once on insert.
CREATE TABLE scope_closure (
    org_id        uuid NOT NULL,
    ancestor_id   uuid NOT NULL REFERENCES scopes (id),
    descendant_id uuid NOT NULL REFERENCES scopes (id),
    distance      integer NOT NULL CHECK (distance >= 0),
    PRIMARY KEY (ancestor_id, descendant_id)
);
CREATE INDEX scope_closure_descendant_idx ON scope_closure (descendant_id, distance);

CREATE TABLE org_settings (
    org_id             uuid PRIMARY KEY REFERENCES scopes (id),
    allowed_levels     text[] NOT NULL DEFAULT ARRAY['organization', 'team', 'project'],
    max_depth          integer NOT NULL DEFAULT 5 CHECK (max_depth BETWEEN 1 AND 16),
    owner_principal_id uuid NOT NULL REFERENCES principals (id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- Derives org_id, depth and path from the parent, enforces max depth and the
-- allowed level labels, and rejects structural updates (no moves in v1).
-- +goose StatementBegin
CREATE FUNCTION tm_scopes_before_write() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    parent   scopes%ROWTYPE;
    settings org_settings%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.parent_id IS DISTINCT FROM OLD.parent_id
           OR NEW.org_id <> OLD.org_id
           OR NEW.slug <> OLD.slug
           OR NEW.depth <> OLD.depth
           OR NEW.path <> OLD.path THEN
            RAISE EXCEPTION 'scopes cannot be moved or re-slugged in v1'
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.parent_id IS NULL THEN
        -- Only an organization root may lack a parent.
        IF NEW.level <> 'organization' THEN
            RAISE EXCEPTION 'only organization roots may have no parent'
                USING ERRCODE = 'check_violation';
        END IF;
        NEW.org_id := NEW.id;
        NEW.depth := 0;
        NEW.path := NEW.slug;
        RETURN NEW;
    END IF;

    SELECT * INTO parent FROM scopes WHERE id = NEW.parent_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'parent scope % does not exist', NEW.parent_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    -- FOR SHARE serializes against ConfigureOrg, which locks the row FOR UPDATE.
    SELECT * INTO settings FROM org_settings WHERE org_id = parent.org_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'organization % has no settings', parent.org_id
            USING ERRCODE = 'check_violation';
    END IF;

    NEW.org_id := parent.org_id;
    NEW.depth := parent.depth + 1;
    NEW.path := parent.path || '/' || NEW.slug;

    -- max_depth counts levels, so the root is level 1 and depth max_depth-1 is the deepest.
    IF NEW.depth >= settings.max_depth THEN
        RAISE EXCEPTION 'scope depth % exceeds organization maximum of % levels', NEW.depth + 1, settings.max_depth
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT (NEW.level = ANY (settings.allowed_levels)) THEN
        RAISE EXCEPTION 'level % is not allowed in this organization', NEW.level
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER scopes_before_write BEFORE INSERT OR UPDATE ON scopes
    FOR EACH ROW EXECUTE FUNCTION tm_scopes_before_write();
CREATE TRIGGER scopes_no_delete BEFORE DELETE ON scopes
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();
CREATE TRIGGER scopes_no_truncate BEFORE TRUNCATE ON scopes
    FOR EACH STATEMENT EXECUTE FUNCTION tm_reject_mutation();

-- +goose StatementBegin
CREATE FUNCTION tm_scopes_after_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO scope_closure (org_id, ancestor_id, descendant_id, distance)
    VALUES (NEW.org_id, NEW.id, NEW.id, 0);
    IF NEW.parent_id IS NOT NULL THEN
        INSERT INTO scope_closure (org_id, ancestor_id, descendant_id, distance)
        SELECT NEW.org_id, ancestor_id, NEW.id, distance + 1
        FROM scope_closure WHERE descendant_id = NEW.parent_id;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER scopes_after_insert AFTER INSERT ON scopes
    FOR EACH ROW EXECUTE FUNCTION tm_scopes_after_insert();
CREATE TRIGGER scope_closure_append_only BEFORE UPDATE OR DELETE ON scope_closure
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();
CREATE TRIGGER scope_closure_no_truncate BEFORE TRUNCATE ON scope_closure
    FOR EACH STATEMENT EXECUTE FUNCTION tm_reject_mutation();

CREATE TABLE scope_members (
    org_id       uuid NOT NULL,
    scope_id     uuid NOT NULL,
    principal_id uuid NOT NULL REFERENCES principals (id),
    role         text NOT NULL CHECK (role IN ('reader', 'writer', 'maintainer')),
    cascade      boolean NOT NULL DEFAULT false,
    granted_by   uuid REFERENCES principals (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_id, principal_id),
    FOREIGN KEY (scope_id, org_id) REFERENCES scopes (id, org_id)
);
CREATE INDEX scope_members_principal_idx ON scope_members (principal_id);

------------------------------------------------------------------------------
-- Memories: current-state projection, immutable revisions, immutable events
------------------------------------------------------------------------------

CREATE TABLE memories (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id                  uuid NOT NULL,
    scope_id                uuid NOT NULL,
    kind                    text NOT NULL CHECK (kind IN ('fact', 'decision', 'convention', 'lesson', 'context')),
    status                  text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'superseded', 'archived')),
    trust                   text NOT NULL CHECK (trust IN ('unverified', 'cited', 'approved')),
    current_revision        integer NOT NULL CHECK (current_revision >= 1),
    -- Incremented by every lifecycle change (revision, archive, supersede, review
    -- outcome) so concurrent lifecycle actions can be detected.
    state_version           bigint NOT NULL DEFAULT 1,
    superseded_by           uuid REFERENCES memories (id),
    -- Promotion provenance (finalization lands in stage 04).
    promoted_from_memory_id uuid REFERENCES memories (id),
    promoted_from_revision  integer,
    created_by              uuid NOT NULL REFERENCES principals (id),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, org_id),
    FOREIGN KEY (scope_id, org_id) REFERENCES scopes (id, org_id),
    CHECK ((status = 'superseded') = (superseded_by IS NOT NULL)),
    CHECK ((promoted_from_memory_id IS NULL) = (promoted_from_revision IS NULL))
);
CREATE INDEX memories_scope_status_idx ON memories (scope_id, status, updated_at DESC);

-- Immutable content snapshots. scope_id/org_id are denormalized so RLS (stage 04)
-- can protect this table directly without a join.
CREATE TABLE memory_revisions (
    memory_id   uuid NOT NULL,
    revision    integer NOT NULL CHECK (revision >= 1),
    org_id      uuid NOT NULL,
    scope_id    uuid NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('fact', 'decision', 'convention', 'lesson', 'context')),
    title       text NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    content     text NOT NULL CHECK (length(content) BETWEEN 1 AND 20000),
    rationale   text NOT NULL DEFAULT '' CHECK (length(rationale) <= 10000),
    source_uri  text CHECK (source_uri IS NULL OR length(source_uri) <= 2048),
    changed_by  uuid NOT NULL REFERENCES principals (id),
    change_note text NOT NULL DEFAULT '' CHECK (length(change_note) <= 1000),
    created_at  timestamptz NOT NULL DEFAULT now(),
    search      tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', title), 'A') ||
        setweight(to_tsvector('english', content), 'B') ||
        setweight(to_tsvector('english', rationale), 'C')
    ) STORED,
    PRIMARY KEY (memory_id, revision),
    FOREIGN KEY (memory_id, org_id) REFERENCES memories (id, org_id),
    FOREIGN KEY (scope_id, org_id) REFERENCES scopes (id, org_id)
);
CREATE INDEX memory_revisions_search_idx ON memory_revisions USING gin (search);
CREATE INDEX memory_revisions_scope_idx ON memory_revisions (scope_id);

-- Override links are part of the immutable content revision. They have no
-- effect until approval-gated behaviour is implemented in stage 04.
CREATE TABLE revision_overrides (
    memory_id            uuid NOT NULL,
    revision             integer NOT NULL,
    org_id               uuid NOT NULL,
    overridden_memory_id uuid NOT NULL REFERENCES memories (id),
    PRIMARY KEY (memory_id, revision, overridden_memory_id),
    FOREIGN KEY (memory_id, revision) REFERENCES memory_revisions (memory_id, revision),
    CHECK (memory_id <> overridden_memory_id)
);

CREATE TRIGGER memory_revisions_immutable BEFORE UPDATE OR DELETE ON memory_revisions
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();
CREATE TRIGGER memory_revisions_no_truncate BEFORE TRUNCATE ON memory_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION tm_reject_mutation();
CREATE TRIGGER revision_overrides_immutable BEFORE UPDATE OR DELETE ON revision_overrides
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();
CREATE TRIGGER revision_overrides_no_truncate BEFORE TRUNCATE ON revision_overrides
    FOR EACH STATEMENT EXECUTE FUNCTION tm_reject_mutation();

-- Immutable lifecycle events bound to a revision. The git mirror (stage 06)
-- exports one commit per exportable event, keyed by id.
CREATE TABLE memory_events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq        bigint NOT NULL GENERATED ALWAYS AS IDENTITY UNIQUE,
    org_id     uuid NOT NULL,
    scope_id   uuid NOT NULL,
    memory_id  uuid NOT NULL,
    revision   integer NOT NULL,
    event_type text NOT NULL CHECK (event_type IN (
        'created', 'revised', 'archived', 'superseded',
        'review_requested', 'approved', 'review_rejected',
        'promotion_submitted', 'promotion_finalized'
    )),
    actor_id   uuid NOT NULL REFERENCES principals (id),
    -- Lifecycle snapshot at the time of the event (status, trust, links), so
    -- delayed consumers never need to read current state.
    snapshot   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (memory_id, revision) REFERENCES memory_revisions (memory_id, revision),
    FOREIGN KEY (scope_id, org_id) REFERENCES scopes (id, org_id)
);
CREATE INDEX memory_events_memory_idx ON memory_events (memory_id, seq);

CREATE TRIGGER memory_events_immutable BEFORE UPDATE OR DELETE ON memory_events
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();
CREATE TRIGGER memory_events_no_truncate BEFORE TRUNCATE ON memory_events
    FOR EACH STATEMENT EXECUTE FUNCTION tm_reject_mutation();

-- Revision-bound review outcomes (behaviour in stages 04/05).
CREATE TABLE approvals (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL,
    memory_id   uuid NOT NULL,
    revision    integer NOT NULL,
    event_id    uuid NOT NULL UNIQUE REFERENCES memory_events (id),
    decision    text NOT NULL CHECK (decision IN ('approved', 'rejected')),
    reviewer_id uuid NOT NULL REFERENCES principals (id),
    note        text NOT NULL DEFAULT '' CHECK (length(note) <= 2000),
    decided_at  timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (memory_id, revision) REFERENCES memory_revisions (memory_id, revision)
);
CREATE INDEX approvals_memory_idx ON approvals (memory_id, revision);
CREATE TRIGGER approvals_immutable BEFORE UPDATE OR DELETE ON approvals
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();
CREATE TRIGGER approvals_no_truncate BEFORE TRUNCATE ON approvals
    FOR EACH STATEMENT EXECUTE FUNCTION tm_reject_mutation();

-- Promotion requests (behaviour in stages 04/05). The snapshot columns hold
-- exactly what destination reviewers may see.
CREATE TABLE promotion_requests (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id               uuid NOT NULL,
    source_memory_id     uuid NOT NULL,
    source_revision      integer NOT NULL,
    source_scope_id      uuid NOT NULL,
    destination_scope_id uuid NOT NULL,
    submitted_by         uuid NOT NULL REFERENCES principals (id),
    rationale            text NOT NULL DEFAULT '' CHECK (length(rationale) <= 2000),
    snapshot             jsonb NOT NULL,
    status               text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'accepted', 'rejected', 'cancelled', 'stale')),
    decided_by           uuid REFERENCES principals (id),
    decided_at           timestamptz,
    result_memory_id     uuid REFERENCES memories (id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (source_memory_id, source_revision) REFERENCES memory_revisions (memory_id, revision),
    FOREIGN KEY (source_scope_id, org_id) REFERENCES scopes (id, org_id),
    FOREIGN KEY (destination_scope_id, org_id) REFERENCES scopes (id, org_id)
);

-- Embeddings (populated from stage 02). Tenant columns allow direct RLS.
CREATE TABLE memory_embeddings (
    memory_id  uuid NOT NULL,
    revision   integer NOT NULL,
    org_id     uuid NOT NULL,
    scope_id   uuid NOT NULL,
    model      text NOT NULL,
    dimensions integer NOT NULL,
    embedding  vector(768) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, revision, model),
    FOREIGN KEY (memory_id, revision) REFERENCES memory_revisions (memory_id, revision),
    FOREIGN KEY (scope_id, org_id) REFERENCES scopes (id, org_id)
);

------------------------------------------------------------------------------
-- Audit, idempotency, jobs, mirror state
------------------------------------------------------------------------------

CREATE TABLE audit_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id       uuid,
    occurred_at  timestamptz NOT NULL DEFAULT now(),
    actor_id     uuid REFERENCES principals (id),
    action       text NOT NULL,
    target_type  text NOT NULL,
    target_id    text NOT NULL,
    scope_id     uuid,
    outcome      text NOT NULL DEFAULT 'success' CHECK (outcome IN ('success', 'denied', 'error')),
    details      jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_events_org_time_idx ON audit_events (org_id, occurred_at DESC);
CREATE TRIGGER audit_events_append_only BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION tm_reject_mutation();

CREATE TABLE idempotency_keys (
    principal_id uuid NOT NULL REFERENCES principals (id),
    key          text NOT NULL CHECK (length(key) BETWEEN 1 AND 200),
    operation    text NOT NULL,
    request_hash bytea NOT NULL,
    response     jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    PRIMARY KEY (principal_id, key)
);

-- Durable outbox for background work. Several job kinds share one table.
CREATE TABLE jobs (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id       uuid,
    kind         text NOT NULL CHECK (kind IN ('embed', 'git_mirror')),
    dedupe_key   text UNIQUE,
    payload      jsonb NOT NULL,
    status       text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'dead')),
    attempts     integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL DEFAULT 10,
    run_after    timestamptz NOT NULL DEFAULT now(),
    locked_at    timestamptz,
    locked_by    text,
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_ready_idx ON jobs (kind, run_after) WHERE status = 'pending';

-- Git mirror delivery state per event and destination repository (stage 06).
CREATE TABLE mirror_state (
    event_id       uuid NOT NULL REFERENCES memory_events (id),
    repository     text NOT NULL,
    org_id         uuid NOT NULL,
    delivery_state text NOT NULL DEFAULT 'pending'
        CHECK (delivery_state IN ('pending', 'committed', 'pushed', 'failed')),
    commit_sha     text,
    attempts       integer NOT NULL DEFAULT 0,
    exported_at    timestamptz,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, repository)
);

-- +goose Down
-- Intentionally irreversible: dropping these tables would destroy history.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'migration 00001_foundation cannot be reversed'; END $$;
-- +goose StatementEnd
