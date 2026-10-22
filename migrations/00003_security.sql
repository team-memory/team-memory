-- +goose Up
-- Stage 04: database roles, precomputed scope access, row-level security on
-- every tenant-owned table, lifecycle guard triggers and the narrow
-- definer functions that cross scope boundaries on purpose.
-- See docs/decisions/rls.md, docs/decisions/trust-rules.md and
-- docs/security/threat-model.md.

------------------------------------------------------------------------------
-- Roles
------------------------------------------------------------------------------
-- Group roles (NOLOGIN). Deployments create login roles that are members:
--   team_memory_app      serve: subject to RLS, never owns tables
--   team_memory_worker   embedding worker: reads only what it embeds
--   team_memory_operator migrations and the operator CLI (table owner)
-- Roles are cluster-wide, so creation tolerates concurrent migrations of
-- other databases in the same cluster (tests do that).
-- +goose StatementBegin
DO $$
DECLARE r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['team_memory_app', 'team_memory_worker', 'team_memory_operator'] LOOP
        BEGIN
            EXECUTE format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE', r);
        EXCEPTION WHEN duplicate_object OR unique_violation THEN
            NULL;
        END;
    END LOOP;
    -- The migrating role owns the tables and acts as operator. A superuser
    -- bypasses RLS anyway; a non-superuser owner needs this membership.
    BEGIN
        EXECUTE format('GRANT team_memory_operator TO %I', current_user);
    EXCEPTION WHEN insufficient_privilege OR invalid_grant_operation OR duplicate_object THEN
        RAISE NOTICE 'could not grant team_memory_operator to %; grant it manually', current_user;
    END;
END;
$$;
-- +goose StatementEnd

-- Nothing but USAGE on the schema for the runtime roles. Only the schema
-- owner may change these; PostgreSQL 15+ already defaults to this.
-- +goose StatementBegin
DO $$
BEGIN
    REVOKE CREATE ON SCHEMA public FROM PUBLIC;
    GRANT USAGE ON SCHEMA public TO team_memory_app, team_memory_worker;
EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'not the owner of schema public; check that PUBLIC has no CREATE on it';
END;
$$;
-- +goose StatementEnd

------------------------------------------------------------------------------
-- Request identity helpers (STABLE, invoker rights, inlinable)
------------------------------------------------------------------------------

-- The acting principal, set per transaction by postgres.WithActorTx with
-- set_config(..., true). Unset, empty or the nil UUID means no principal:
-- every policy that depends on it then matches nothing.
CREATE FUNCTION tm_principal() RETURNS uuid
LANGUAGE sql STABLE PARALLEL SAFE AS
$$ SELECT NULLIF(NULLIF(current_setting('app.principal_id', true), '')::uuid,
                 '00000000-0000-0000-0000-000000000000'::uuid) $$;

-- How the acting principal authenticated: pat, oauth or portal. Review
-- completion (approval, rejection, promotion) requires 'portal'. The app
-- role sets this itself, so it is a guard against Go bugs, not against
-- arbitrary SQL from a compromised app process (see threat model).
CREATE FUNCTION tm_auth_method() RETURNS text
LANGUAGE sql STABLE PARALLEL SAFE AS
$$ SELECT coalesce(current_setting('app.auth_method', true), '') $$;

-- True for the operator role (and superusers). Used by guard triggers;
-- policies use TO team_memory_operator instead.
CREATE FUNCTION tm_is_operator() RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE AS
$$ SELECT pg_has_role(current_user, 'team_memory_operator', 'USAGE') $$;

CREATE FUNCTION tm_role_rank(role text) RETURNS smallint
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS
$$ SELECT CASE role WHEN 'reader' THEN 1 WHEN 'writer' THEN 2 WHEN 'maintainer' THEN 3 ELSE 0 END::smallint $$;

------------------------------------------------------------------------------
-- Precomputed scope access
------------------------------------------------------------------------------
-- One row per (principal, scope) the principal can read:
--   rank 1..3     effective role on the scope (direct membership or a
--                 cascading grant on an ancestor): own memories readable,
--                 writable from rank 2, maintainable at rank 3
--   rank 0        readable only as inherited: an ancestor of a scope where
--                 the principal has a role
-- cascade_rank is the strongest role from grants that also cover every
-- descendant (a cascading grant on the scope or an ancestor). Granting a
-- cascading role requires cascade_rank 3, so a maintainer of one scope
-- cannot open its subtree to others.
-- Descendants are never readable through an ancestor membership: they
-- appear only via a cascading grant, with rank >= 1.
-- Maintained by triggers on scope_members and scopes, recomputed per
-- principal under an advisory lock, so a revocation is visible to the next
-- transaction after it commits.
CREATE TABLE scope_access (
    principal_id uuid NOT NULL REFERENCES principals (id),
    scope_id     uuid NOT NULL,
    org_id       uuid NOT NULL,
    rank         smallint NOT NULL CHECK (rank BETWEEN 0 AND 3),
    cascade_rank smallint NOT NULL DEFAULT 0 CHECK (cascade_rank BETWEEN 0 AND 3),
    PRIMARY KEY (principal_id, scope_id),
    FOREIGN KEY (scope_id, org_id) REFERENCES scopes (id, org_id),
    CHECK (cascade_rank <= rank)
);
CREATE INDEX scope_access_scope_idx ON scope_access (scope_id);

-- +goose StatementBegin
CREATE FUNCTION tm_refresh_scope_access(p uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('tm_scope_access|' || p::text, 0));
    DELETE FROM scope_access WHERE principal_id = p;
    INSERT INTO scope_access (principal_id, scope_id, org_id, rank, cascade_rank)
    WITH granted AS (
        SELECT c.descendant_id AS scope_id, c.org_id,
               max(tm_role_rank(m.role)) AS rank,
               max(CASE WHEN m.cascade THEN tm_role_rank(m.role) ELSE 0 END) AS cascade_rank
        FROM scope_members m
        JOIN scope_closure c ON c.ancestor_id = m.scope_id
        WHERE m.principal_id = p AND (c.distance = 0 OR m.cascade)
        GROUP BY c.descendant_id, c.org_id
    ), inherited AS (
        SELECT DISTINCT c.ancestor_id AS scope_id, c.org_id
        FROM granted g JOIN scope_closure c ON c.descendant_id = g.scope_id
        WHERE c.distance > 0
    )
    SELECT p, g.scope_id, g.org_id, g.rank, g.cascade_rank FROM granted g
    UNION ALL
    SELECT p, i.scope_id, i.org_id, 0, 0 FROM inherited i
    WHERE NOT EXISTS (SELECT 1 FROM granted g WHERE g.scope_id = i.scope_id);
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION tm_refresh_scope_access(uuid) FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION tm_scope_members_after_write() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        PERFORM tm_refresh_scope_access(OLD.principal_id);
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') AND (TG_OP = 'INSERT' OR NEW.principal_id <> OLD.principal_id) THEN
        PERFORM tm_refresh_scope_access(NEW.principal_id);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER scope_members_after_write AFTER INSERT OR UPDATE OR DELETE ON scope_members
    FOR EACH ROW EXECUTE FUNCTION tm_scope_members_after_write();

-- A new scope is readable by everyone holding a cascading grant on one of
-- its ancestors. Runs after scopes_after_insert (trigger names sort), which
-- writes the closure rows.
-- +goose StatementBegin
CREATE FUNCTION tm_scopes_after_insert_access() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE p uuid;
BEGIN
    FOR p IN
        SELECT DISTINCT m.principal_id
        FROM scope_members m JOIN scope_closure c ON c.ancestor_id = m.scope_id
        WHERE c.descendant_id = NEW.id AND c.distance > 0 AND m.cascade
    LOOP
        PERFORM tm_refresh_scope_access(p);
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER scopes_after_insert_access AFTER INSERT ON scopes
    FOR EACH ROW EXECUTE FUNCTION tm_scopes_after_insert_access();

-- Tree maintenance reads parents and settings and writes closure rows on the
-- caller's behalf; the INSERT policy on scopes decides who may create one.
ALTER FUNCTION tm_scopes_before_write() SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION tm_scopes_after_insert() SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp;

-- Backfill for existing memberships.
-- +goose StatementBegin
DO $$
DECLARE p uuid;
BEGIN
    FOR p IN SELECT DISTINCT principal_id FROM scope_members LOOP
        PERFORM tm_refresh_scope_access(p);
    END LOOP;
END;
$$;
-- +goose StatementEnd

------------------------------------------------------------------------------
-- Schema additions
------------------------------------------------------------------------------

-- Memberships written by IdP group mapping are marked so login sync only
-- ever touches its own rows.
ALTER TABLE scope_members ADD COLUMN source text NOT NULL DEFAULT 'manual'
    CHECK (source IN ('manual', 'idp'));

-- Optional IdP group to role mapping, per organization (operator managed).
CREATE TABLE idp_group_mappings (
    org_id     uuid NOT NULL,
    issuer     text NOT NULL,
    group_name text NOT NULL CHECK (length(group_name) BETWEEN 1 AND 500),
    scope_id   uuid NOT NULL,
    role       text NOT NULL CHECK (role IN ('reader', 'writer', 'maintainer')),
    cascade    boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, group_name, scope_id),
    FOREIGN KEY (scope_id, org_id) REFERENCES scopes (id, org_id)
);

-- Rows that RLS protects by scope_id must agree with their memory's scope.
ALTER TABLE memories ADD CONSTRAINT memories_id_scope_key UNIQUE (id, scope_id);
ALTER TABLE memory_revisions ADD CONSTRAINT memory_revisions_memory_scope_fk
    FOREIGN KEY (memory_id, scope_id) REFERENCES memories (id, scope_id);
ALTER TABLE memory_events ADD CONSTRAINT memory_events_memory_scope_fk
    FOREIGN KEY (memory_id, scope_id) REFERENCES memories (id, scope_id);
ALTER TABLE memory_embeddings ADD CONSTRAINT memory_embeddings_memory_scope_fk
    FOREIGN KEY (memory_id, scope_id) REFERENCES memories (id, scope_id);
ALTER TABLE conflict_candidates
    ADD CONSTRAINT conflict_candidates_memory_scope_fk
        FOREIGN KEY (memory_id, scope_id) REFERENCES memories (id, scope_id),
    ADD CONSTRAINT conflict_candidates_candidate_scope_fk
        FOREIGN KEY (candidate_memory_id, candidate_scope_id) REFERENCES memories (id, scope_id);
ALTER TABLE promotion_requests ADD CONSTRAINT promotion_requests_source_scope_fk
    FOREIGN KEY (source_memory_id, source_scope_id) REFERENCES memories (id, scope_id);

-- Append-only tables gain scope_id for direct policies; the backfill is the
-- one sanctioned update, made by the migration with the guard disabled.
ALTER TABLE revision_overrides ADD COLUMN scope_id uuid;
ALTER TABLE approvals ADD COLUMN scope_id uuid;
ALTER TABLE revision_overrides DISABLE TRIGGER revision_overrides_immutable;
ALTER TABLE approvals DISABLE TRIGGER approvals_immutable;
UPDATE revision_overrides o SET scope_id = m.scope_id FROM memories m WHERE m.id = o.memory_id;
UPDATE approvals a SET scope_id = m.scope_id FROM memories m WHERE m.id = a.memory_id;
ALTER TABLE revision_overrides ENABLE TRIGGER revision_overrides_immutable;
ALTER TABLE approvals ENABLE TRIGGER approvals_immutable;
ALTER TABLE revision_overrides ALTER COLUMN scope_id SET NOT NULL,
    ADD CONSTRAINT revision_overrides_memory_scope_fk FOREIGN KEY (memory_id, scope_id) REFERENCES memories (id, scope_id);
ALTER TABLE approvals ALTER COLUMN scope_id SET NOT NULL,
    ADD CONSTRAINT approvals_memory_scope_fk FOREIGN KEY (memory_id, scope_id) REFERENCES memories (id, scope_id);
CREATE INDEX approvals_latest_idx ON approvals (memory_id, revision, decided_at DESC, id DESC);

-- memory_approve is idempotent per exact revision.
CREATE UNIQUE INDEX memory_events_review_requested_idx ON memory_events (memory_id, revision)
    WHERE event_type = 'review_requested';

-- At most one live promotion of a source revision.
CREATE UNIQUE INDEX promotion_requests_live_idx ON promotion_requests (source_memory_id, source_revision)
    WHERE status IN ('pending', 'accepted');
CREATE INDEX promotion_requests_destination_idx ON promotion_requests (destination_scope_id, status);

-- Audit rows carry the request ID that produced them.
ALTER TABLE audit_events ADD COLUMN request_id text CHECK (request_id IS NULL OR length(request_id) <= 100);
CREATE INDEX audit_events_scope_time_idx ON audit_events (scope_id, occurred_at DESC);

------------------------------------------------------------------------------
-- Guard triggers (invoker rights; the operator role is exempt)
------------------------------------------------------------------------------
-- Like the definer functions, they pin search_path with pg_temp last:
-- otherwise PostgreSQL resolves table names in pg_temp first, and a caller
-- could shadow approvals or scope_access with a temporary table.

-- Lifecycle rules for the memories projection. RLS decides which rows a
-- principal may update at all (writer on the owning scope); this trigger
-- decides which transitions are legal.
-- +goose StatementBegin
CREATE FUNCTION tm_memories_before_update() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    latest text;
    maintainer boolean;
BEGIN
    IF tm_is_operator() THEN
        RETURN NEW;
    END IF;
    IF NEW.id <> OLD.id OR NEW.org_id <> OLD.org_id OR NEW.scope_id <> OLD.scope_id
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at
       OR NEW.promoted_from_memory_id IS DISTINCT FROM OLD.promoted_from_memory_id
       OR NEW.promoted_from_revision IS DISTINCT FROM OLD.promoted_from_revision THEN
        RAISE EXCEPTION 'memory identity, scope and provenance cannot change'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.state_version <> OLD.state_version + 1 THEN
        RAISE EXCEPTION 'every memory change must advance state_version by one'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.status <> 'active' THEN
        RAISE EXCEPTION 'memory is not active' USING ERRCODE = 'check_violation';
    END IF;
    maintainer := NEW.scope_id IN (SELECT a.scope_id FROM scope_access a
                                   WHERE a.principal_id = tm_principal() AND a.rank >= 3);

    IF NEW.status <> OLD.status THEN
        IF NOT maintainer THEN
            RAISE EXCEPTION 'archiving or superseding needs maintainer on the owning scope'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        IF NEW.status NOT IN ('archived', 'superseded') OR NEW.current_revision <> OLD.current_revision
           OR NEW.trust <> OLD.trust THEN
            RAISE EXCEPTION 'invalid lifecycle transition' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.superseded_by IS DISTINCT FROM OLD.superseded_by THEN
        RAISE EXCEPTION 'superseded_by changes only with supersession' USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.current_revision <> OLD.current_revision THEN
        -- A new content revision always resets trust: approval is bound to
        -- an exact revision.
        IF NEW.current_revision <> OLD.current_revision + 1 OR NEW.trust = 'approved' THEN
            RAISE EXCEPTION 'a new revision starts unapproved' USING ERRCODE = 'check_violation';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM memory_revisions r
                       WHERE r.memory_id = NEW.id AND r.revision = NEW.current_revision) THEN
            RAISE EXCEPTION 'revision % does not exist', NEW.current_revision USING ERRCODE = 'foreign_key_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.trust <> OLD.trust THEN
        SELECT a.decision INTO latest FROM approvals a
        WHERE a.memory_id = NEW.id AND a.revision = NEW.current_revision
        ORDER BY a.decided_at DESC, a.id DESC LIMIT 1;
        IF NEW.trust = 'approved' AND latest IS DISTINCT FROM 'approved' THEN
            RAISE EXCEPTION 'approved trust needs an approval of the current revision'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        IF OLD.trust = 'approved' AND latest IS DISTINCT FROM 'rejected' THEN
            RAISE EXCEPTION 'approval is withdrawn only by a rejection of the current revision'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        IF OLD.trust <> 'approved' AND NEW.trust <> 'approved' THEN
            RAISE EXCEPTION 'trust changes only with a revision or a review outcome'
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER memories_before_update BEFORE UPDATE ON memories
    FOR EACH ROW EXECUTE FUNCTION tm_memories_before_update();
CREATE TRIGGER memories_no_delete BEFORE DELETE ON memories
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();

-- Memberships: identity columns are fixed, and the organization owner's
-- root grant can only be changed by an operator.
-- +goose StatementBegin
CREATE FUNCTION tm_scope_members_before_change() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF tm_is_operator() THEN
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    IF TG_OP = 'UPDATE' AND (NEW.principal_id <> OLD.principal_id OR NEW.scope_id <> OLD.scope_id
                             OR NEW.org_id <> OLD.org_id) THEN
        RAISE EXCEPTION 'membership principal and scope cannot change' USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM org_settings s
               WHERE s.org_id = OLD.scope_id AND s.owner_principal_id = OLD.principal_id) THEN
        RAISE EXCEPTION 'the organization owner''s root grant is changed only by an operator'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER scope_members_before_change BEFORE UPDATE OR DELETE ON scope_members
    FOR EACH ROW EXECUTE FUNCTION tm_scope_members_before_change();

-- Promotion requests move only from pending, to an outcome the actor may
-- choose. Acceptance happens only inside tm_promotion_finalize_source.
-- +goose StatementBegin
CREATE FUNCTION tm_promotion_requests_before_update() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    me uuid := tm_principal();
BEGIN
    IF tm_is_operator() THEN
        RETURN NEW;
    END IF;
    IF NEW.id <> OLD.id OR NEW.org_id <> OLD.org_id OR NEW.source_memory_id <> OLD.source_memory_id
       OR NEW.source_revision <> OLD.source_revision OR NEW.source_scope_id <> OLD.source_scope_id
       OR NEW.destination_scope_id <> OLD.destination_scope_id OR NEW.submitted_by <> OLD.submitted_by
       OR NEW.rationale <> OLD.rationale OR NEW.snapshot <> OLD.snapshot OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'promotion request content is immutable' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'promotion request is already %', OLD.status USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.decided_by IS DISTINCT FROM me OR NEW.decided_at IS NULL THEN
        RAISE EXCEPTION 'the deciding principal must be recorded' USING ERRCODE = 'check_violation';
    END IF;
    CASE NEW.status
        WHEN 'rejected' THEN
            IF tm_auth_method() <> 'portal' OR NOT EXISTS (
                SELECT 1 FROM scope_access a WHERE a.principal_id = me
                  AND a.scope_id = OLD.destination_scope_id AND a.rank >= 3) THEN
                RAISE EXCEPTION 'only a destination maintainer in a browser review may reject'
                    USING ERRCODE = 'insufficient_privilege';
            END IF;
        WHEN 'cancelled' THEN
            IF OLD.submitted_by <> me AND NOT EXISTS (
                SELECT 1 FROM scope_access a WHERE a.principal_id = me
                  AND a.scope_id = OLD.source_scope_id AND a.rank >= 3) THEN
                RAISE EXCEPTION 'only the submitter or a source maintainer may cancel'
                    USING ERRCODE = 'insufficient_privilege';
            END IF;
        WHEN 'stale' THEN
            NULL;
        ELSE
            RAISE EXCEPTION 'promotion requests are accepted only by finalization'
                USING ERRCODE = 'insufficient_privilege';
    END CASE;
    IF NEW.result_memory_id IS NOT NULL THEN
        RAISE EXCEPTION 'only finalization sets the result' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER promotion_requests_before_update BEFORE UPDATE ON promotion_requests
    FOR EACH ROW EXECUTE FUNCTION tm_promotion_requests_before_update();
CREATE TRIGGER promotion_requests_no_delete BEFORE DELETE ON promotion_requests
    FOR EACH ROW EXECUTE FUNCTION tm_reject_mutation();

------------------------------------------------------------------------------
-- Narrow definer functions
------------------------------------------------------------------------------
-- Every definer function pins search_path = pg_catalog, public, pg_temp.
-- pg_temp must be named, and last: when it is omitted PostgreSQL searches
-- it first for tables, so any role allowed to create temporary tables
-- (PUBLIC by default) could make the function read a shadow table.

-- Promotion finalization, source side. The destination reviewer cannot read
-- or change source memories through RLS, so the source supersession runs
-- here, after rechecking every condition of the accepted contract inside
-- the caller's transaction: browser review, human reviewer with maintainer
-- on the destination, destination is the source's parent, the source is
-- still active at the submitted revision and approved there, and the
-- submitter still maintains the source scope. The caller has already
-- created the destination memory (p_result) with an approval.
-- SQLSTATE TMS01 means stale: the caller marks the request stale and the
-- submitter must resubmit.
-- +goose StatementBegin
CREATE FUNCTION tm_promotion_finalize_source(p_request uuid, p_result uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    me     uuid := tm_principal();
    r      promotion_requests%ROWTYPE;
    src    memories%ROWTYPE;
    dst    memories%ROWTYPE;
    latest text;
BEGIN
    IF me IS NULL OR tm_auth_method() <> 'portal' THEN
        RAISE EXCEPTION 'promotion finalization requires a browser review' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM principals WHERE id = me AND kind = 'human' AND disabled_at IS NULL) THEN
        RAISE EXCEPTION 'only an enabled human principal may finalize a promotion' USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT * INTO r FROM promotion_requests WHERE id = p_request FOR UPDATE;
    IF NOT FOUND OR NOT EXISTS (SELECT 1 FROM scope_access a WHERE a.principal_id = me
                                AND a.scope_id = r.destination_scope_id AND a.rank >= 3) THEN
        RAISE EXCEPTION 'promotion request not found' USING ERRCODE = 'no_data_found';
    END IF;
    IF r.status <> 'pending' THEN
        RAISE EXCEPTION 'promotion request is %', r.status USING ERRCODE = 'TMS02';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM scopes s WHERE s.id = r.source_scope_id AND s.parent_id = r.destination_scope_id) THEN
        RAISE EXCEPTION 'promotion must target the source scope''s parent' USING ERRCODE = 'check_violation';
    END IF;

    SELECT * INTO src FROM memories WHERE id = r.source_memory_id FOR UPDATE;
    SELECT a.decision INTO latest FROM approvals a
    WHERE a.memory_id = src.id AND a.revision = r.source_revision
    ORDER BY a.decided_at DESC, a.id DESC LIMIT 1;
    IF src.scope_id <> r.source_scope_id OR src.status <> 'active'
       OR src.current_revision <> r.source_revision OR src.trust <> 'approved'
       OR latest IS DISTINCT FROM 'approved'
       OR NOT EXISTS (SELECT 1 FROM scope_access a JOIN principals p ON p.id = a.principal_id
                      WHERE a.principal_id = r.submitted_by AND a.scope_id = r.source_scope_id
                        AND a.rank >= 3 AND p.disabled_at IS NULL) THEN
        RAISE EXCEPTION 'promotion request is stale' USING ERRCODE = 'TMS01';
    END IF;

    SELECT * INTO dst FROM memories WHERE id = p_result;
    IF NOT FOUND OR dst.scope_id <> r.destination_scope_id OR dst.created_by <> me
       OR dst.promoted_from_memory_id IS DISTINCT FROM src.id
       OR dst.promoted_from_revision IS DISTINCT FROM r.source_revision
       OR dst.status <> 'active' OR dst.trust <> 'approved' THEN
        RAISE EXCEPTION 'promotion result does not match the request' USING ERRCODE = 'check_violation';
    END IF;

    UPDATE memories
    SET status = 'superseded', superseded_by = dst.id, state_version = state_version + 1, updated_at = now()
    WHERE id = src.id;
    INSERT INTO memory_events (org_id, scope_id, memory_id, revision, event_type, actor_id, snapshot)
    VALUES (src.org_id, src.scope_id, src.id, src.current_revision, 'superseded', me,
            jsonb_build_object('status', 'superseded', 'trust', src.trust, 'kind', src.kind,
                               'revision', src.current_revision, 'state_version', src.state_version + 1,
                               'superseded_by', dst.id, 'replacement', dst.id, 'reason', 'promotion',
                               'promotion_request', r.id));
    UPDATE promotion_requests
    SET status = 'accepted', decided_by = me, decided_at = now(), result_memory_id = dst.id
    WHERE id = r.id;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION tm_promotion_finalize_source(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tm_promotion_finalize_source(uuid, uuid) TO team_memory_app;

-- IdP group sync at sign-in. Grants come only from operator-managed
-- idp_group_mappings, so the app can cause no grant an operator did not
-- configure for that issuer; it can only claim group membership, which is
-- the same trust it has when setting the request identity.
-- +goose StatementBegin
CREATE FUNCTION tm_sync_idp_grants(p_principal uuid, p_issuer text, p_groups text[]) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF p_principal IS NULL OR p_principal IS DISTINCT FROM tm_principal() THEN
        RAISE EXCEPTION 'group sync runs as the signing-in principal' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- Remove IdP-sourced grants this issuer no longer justifies.
    DELETE FROM scope_members m
    WHERE m.principal_id = p_principal AND m.source = 'idp'
      AND NOT EXISTS (SELECT 1 FROM idp_group_mappings g
                      WHERE g.issuer = p_issuer AND g.scope_id = m.scope_id
                        AND g.group_name = ANY (p_groups));
    -- Add or update the strongest mapped grant per scope; manual grants win.
    INSERT INTO scope_members (org_id, scope_id, principal_id, role, cascade, granted_by, source)
    SELECT DISTINCT ON (g.scope_id) g.org_id, g.scope_id, p_principal, g.role, g.cascade, NULL, 'idp'
    FROM idp_group_mappings g
    WHERE g.issuer = p_issuer AND g.group_name = ANY (p_groups)
    ORDER BY g.scope_id, tm_role_rank(g.role) DESC, g.cascade DESC
    ON CONFLICT (scope_id, principal_id) DO UPDATE
        SET role = EXCLUDED.role, cascade = EXCLUDED.cascade, updated_at = now()
        WHERE scope_members.source = 'idp'
          AND (scope_members.role, scope_members.cascade) IS DISTINCT FROM (EXCLUDED.role, EXCLUDED.cascade);
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION tm_sync_idp_grants(uuid, text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tm_sync_idp_grants(uuid, text, text[]) TO team_memory_app;

------------------------------------------------------------------------------
-- Row-level security
------------------------------------------------------------------------------
-- Every tenant-owned table: RLS enabled and forced (the owner is subject to
-- it too), an operator policy, then role-specific policies. Policies use
-- uncorrelated IN (subquery) forms over scope_access, which PostgreSQL
-- evaluates once per statement as a hashed subplan.

-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'scopes', 'scope_closure', 'org_settings', 'scope_members', 'scope_access', 'idp_group_mappings',
        'memories', 'memory_revisions', 'revision_overrides', 'memory_events', 'approvals',
        'promotion_requests', 'memory_embeddings', 'conflict_candidates', 'jobs', 'mirror_state',
        'audit_events', 'idempotency_keys'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY operator_all ON %I TO team_memory_operator USING (true) WITH CHECK (true)', t);
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- scope_access: a principal sees only its own rows. No recursion: this
-- policy reads no other protected table.
CREATE POLICY app_select ON scope_access FOR SELECT TO team_memory_app
    USING (principal_id = tm_principal());

-- scopes: readable scopes only. Creating a child needs a cascading
-- maintainer grant covering the parent (so the creator keeps access).
CREATE POLICY app_select ON scopes FOR SELECT TO team_memory_app
    USING (id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY app_insert ON scopes FOR INSERT TO team_memory_app
    WITH CHECK (created_by = tm_principal()
                AND parent_id IN (SELECT a.scope_id FROM scope_access a
                                  WHERE a.principal_id = tm_principal() AND a.cascade_rank >= 3));
CREATE POLICY worker_select ON scopes FOR SELECT TO team_memory_worker USING (true);

CREATE POLICY app_select ON scope_closure FOR SELECT TO team_memory_app
    USING (descendant_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal())
           AND ancestor_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY worker_select ON scope_closure FOR SELECT TO team_memory_worker USING (true);

CREATE POLICY app_select ON org_settings FOR SELECT TO team_memory_app
    USING (org_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));

-- Memberships: your own, plus every membership of scopes you maintain.
-- Granting needs maintainer on the scope; a cascading grant also needs a
-- cascading maintainer grant covering it.
CREATE POLICY app_select ON scope_members FOR SELECT TO team_memory_app
    USING (principal_id = tm_principal()
           OR scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3));
CREATE POLICY app_insert ON scope_members FOR INSERT TO team_memory_app
    WITH CHECK (granted_by = tm_principal() AND source = 'manual'
                AND scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3)
                AND (NOT cascade OR scope_id IN (SELECT a.scope_id FROM scope_access a
                                                 WHERE a.principal_id = tm_principal() AND a.cascade_rank >= 3)));
CREATE POLICY app_update ON scope_members FOR UPDATE TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3))
    WITH CHECK (granted_by = tm_principal() AND source = 'manual'
                AND scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3)
                AND (NOT cascade OR scope_id IN (SELECT a.scope_id FROM scope_access a
                                                 WHERE a.principal_id = tm_principal() AND a.cascade_rank >= 3)));
CREATE POLICY app_delete ON scope_members FOR DELETE TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3));

-- Group mappings are operator data; only the definer sync function reads them.

-- memories: readable scopes; writers create and revise in their own scopes.
-- The guard trigger enforces lifecycle transitions and maintainer actions.
CREATE POLICY app_select ON memories FOR SELECT TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY app_insert ON memories FOR INSERT TO team_memory_app
    WITH CHECK (created_by = tm_principal() AND status = 'active' AND trust <> 'approved'
                AND current_revision = 1 AND state_version = 1 AND superseded_by IS NULL
                AND scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 2)
                AND (promoted_from_memory_id IS NULL OR EXISTS (
                        SELECT 1 FROM promotion_requests pr
                        WHERE pr.source_memory_id = memories.promoted_from_memory_id
                          AND pr.source_revision = memories.promoted_from_revision
                          AND pr.destination_scope_id = memories.scope_id
                          AND pr.status = 'pending')));
CREATE POLICY app_update ON memories FOR UPDATE TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 2))
    WITH CHECK (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 2));
-- The worker reads a few columns and takes FOR SHARE locks (which need an
-- UPDATE policy); WITH CHECK (false) makes any actual update fail.
CREATE POLICY worker_select ON memories FOR SELECT TO team_memory_worker USING (true);
CREATE POLICY worker_lock ON memories FOR UPDATE TO team_memory_worker USING (true) WITH CHECK (false);

CREATE POLICY app_select ON memory_revisions FOR SELECT TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY app_insert ON memory_revisions FOR INSERT TO team_memory_app
    WITH CHECK (changed_by = tm_principal()
                AND scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 2));
CREATE POLICY worker_select ON memory_revisions FOR SELECT TO team_memory_worker USING (true);

-- An override may target only a memory the author can read, owned by a
-- strict ancestor of the overriding memory's scope.
CREATE POLICY app_select ON revision_overrides FOR SELECT TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY app_insert ON revision_overrides FOR INSERT TO team_memory_app
    WITH CHECK (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 2)
                AND EXISTS (SELECT 1 FROM memories t
                            JOIN scope_closure c ON c.ancestor_id = t.scope_id
                            WHERE t.id = revision_overrides.overridden_memory_id
                              AND c.descendant_id = revision_overrides.scope_id AND c.distance > 0));

-- Lifecycle events: writers record content events; maintainers record
-- archive, supersede and review requests; review outcomes and promotion
-- events also need a browser review session.
CREATE POLICY app_select ON memory_events FOR SELECT TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY app_insert ON memory_events FOR INSERT TO team_memory_app
    WITH CHECK (actor_id = tm_principal()
                AND scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 2)
                AND (event_type IN ('created', 'revised')
                     OR (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3)
                         AND (event_type IN ('archived', 'superseded', 'review_requested')
                              OR (tm_auth_method() = 'portal'
                                  AND event_type IN ('approved', 'review_rejected', 'promotion_submitted', 'promotion_finalized'))))));

-- Review outcomes: a human maintainer of the owning scope, in a browser
-- review, for the memory's current revision.
CREATE POLICY app_select ON approvals FOR SELECT TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY app_insert ON approvals FOR INSERT TO team_memory_app
    WITH CHECK (reviewer_id = tm_principal() AND tm_auth_method() = 'portal'
                AND scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3)
                AND EXISTS (SELECT 1 FROM principals p WHERE p.id = tm_principal() AND p.kind = 'human' AND p.disabled_at IS NULL)
                AND EXISTS (SELECT 1 FROM memories m WHERE m.id = approvals.memory_id
                              AND m.current_revision = approvals.revision AND m.status = 'active'));

-- Promotion requests: visible to the submitter and to maintainers of the
-- source or destination. Destination maintainers see only the request row
-- (its snapshot), never the source memory itself.
CREATE POLICY app_select ON promotion_requests FOR SELECT TO team_memory_app
    USING (submitted_by = tm_principal()
           OR source_scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3)
           OR destination_scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3));
CREATE POLICY app_insert ON promotion_requests FOR INSERT TO team_memory_app
    WITH CHECK (submitted_by = tm_principal() AND status = 'pending' AND tm_auth_method() = 'portal'
                AND decided_by IS NULL AND result_memory_id IS NULL
                AND EXISTS (SELECT 1 FROM principals p WHERE p.id = tm_principal() AND p.kind = 'human' AND p.disabled_at IS NULL)
                AND source_scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3)
                AND EXISTS (SELECT 1 FROM scopes s WHERE s.id = promotion_requests.source_scope_id
                              AND s.parent_id = promotion_requests.destination_scope_id)
                AND EXISTS (SELECT 1 FROM memories m WHERE m.id = promotion_requests.source_memory_id
                              AND m.current_revision = promotion_requests.source_revision
                              AND m.status = 'active' AND m.trust = 'approved'));
CREATE POLICY app_update ON promotion_requests FOR UPDATE TO team_memory_app
    USING (submitted_by = tm_principal()
           OR source_scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3)
           OR destination_scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3));

-- Embeddings: readable with their memory; written only by the worker.
CREATE POLICY app_select ON memory_embeddings FOR SELECT TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY worker_all ON memory_embeddings TO team_memory_worker USING (true) WITH CHECK (true);

-- Conflict candidates: both memories must be readable.
CREATE POLICY app_select ON conflict_candidates FOR SELECT TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal())
           AND candidate_scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY app_update ON conflict_candidates FOR UPDATE TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3)
           AND candidate_scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal()));
CREATE POLICY worker_all ON conflict_candidates TO team_memory_worker USING (true) WITH CHECK (true);

-- Jobs: the app only enqueues embedding jobs for memories it may write.
-- INSERT ... ON CONFLICT also checks the new row against SELECT policies,
-- so the app may see embed jobs of memories it can read (identifiers only;
-- no tool exposes them). The worker owns the queue.
CREATE POLICY app_select ON jobs FOR SELECT TO team_memory_app
    USING (kind = 'embed'
           AND (payload->>'memory_id')::uuid IN (
               SELECT m.id FROM memories m
               WHERE m.scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal())));
CREATE POLICY app_insert ON jobs FOR INSERT TO team_memory_app
    WITH CHECK (kind = 'embed'
                AND (payload->>'memory_id')::uuid IN (
                    SELECT m.id FROM memories m
                    WHERE m.scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 2)));
CREATE POLICY worker_all ON jobs TO team_memory_worker USING (true) WITH CHECK (true);

CREATE POLICY worker_all ON mirror_state TO team_memory_worker USING (true) WITH CHECK (true);

-- Audit: maintainers read their scopes' events. Rows are appended by the
-- acting principal (or with no actor, for authentication failures).
CREATE POLICY app_select ON audit_events FOR SELECT TO team_memory_app
    USING (scope_id IN (SELECT a.scope_id FROM scope_access a WHERE a.principal_id = tm_principal() AND a.rank >= 3));
CREATE POLICY app_insert ON audit_events FOR INSERT TO team_memory_app
    WITH CHECK (actor_id IS NULL OR actor_id = tm_principal());

CREATE POLICY app_all ON idempotency_keys TO team_memory_app
    USING (principal_id = tm_principal()) WITH CHECK (principal_id = tm_principal());

------------------------------------------------------------------------------
-- Privileges
------------------------------------------------------------------------------
-- Least privilege per role. No DELETE on history, no TRUNCATE anywhere, no
-- DDL. Identity tables (principals, identities, tokens, OAuth state) are not
-- tenant-owned; they are protected by privileges, not RLS.

GRANT SELECT ON scopes, scope_closure, org_settings, scope_access, embedding_models,
    principals, principal_identities TO team_memory_app;
GRANT INSERT ON scopes TO team_memory_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON scope_members TO team_memory_app;
GRANT SELECT, INSERT, UPDATE ON memories TO team_memory_app;
GRANT SELECT, INSERT ON memory_revisions, revision_overrides, memory_events, approvals, audit_events TO team_memory_app;
GRANT SELECT, INSERT, UPDATE ON promotion_requests TO team_memory_app;
GRANT SELECT ON memory_embeddings TO team_memory_app;
GRANT SELECT ON conflict_candidates TO team_memory_app;
GRANT UPDATE (status) ON conflict_candidates TO team_memory_app;
GRANT INSERT ON jobs TO team_memory_app;
-- ON CONFLICT (dedupe_key) needs SELECT on the conflict column.
GRANT SELECT (dedupe_key) ON jobs TO team_memory_app;
GRANT SELECT, INSERT, DELETE ON idempotency_keys TO team_memory_app;
GRANT INSERT ON principals, principal_identities TO team_memory_app;
GRANT SELECT, INSERT ON personal_access_tokens TO team_memory_app;
GRANT UPDATE (last_used_at) ON personal_access_tokens TO team_memory_app;

GRANT SELECT ON scopes, scope_closure, embedding_models TO team_memory_worker;
GRANT SELECT (id, org_id, scope_id, kind, status, current_revision) ON memories TO team_memory_worker;
GRANT UPDATE (updated_at) ON memories TO team_memory_worker;
GRANT SELECT (memory_id, revision, org_id, scope_id, kind, title, content, rationale) ON memory_revisions TO team_memory_worker;
GRANT SELECT, INSERT, DELETE ON memory_embeddings TO team_memory_worker;
GRANT SELECT, INSERT ON conflict_candidates TO team_memory_worker;
GRANT SELECT, INSERT, UPDATE ON jobs TO team_memory_worker;
GRANT SELECT, INSERT, UPDATE ON mirror_state TO team_memory_worker;

GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO team_memory_app, team_memory_worker;

-- A separate operator login (not the owner) gets ordinary DML; the
-- append-only triggers still apply to it. Later migrations that add tables
-- must extend these grants and the RLS list above.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO team_memory_operator;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO team_memory_operator;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'migration 00003_security cannot be reversed'; END $$;
-- +goose StatementEnd
