-- +goose Up
-- IdP-sourced grants record the issuer that justified them, so signing in
-- through one provider only adds, changes or removes that provider's
-- grants. Before this, a principal linked to two providers that both sync
-- group roles lost provider B's grants at every sign-in through provider A.

ALTER TABLE scope_members ADD COLUMN idp_issuer text
    CHECK (idp_issuer IS NULL OR length(idp_issuer) <= 2000);
-- Manual grants never carry an issuer. IdP grants written before this
-- migration have none; the next sync through any issuer treats them as
-- unowned (removing them unless that issuer maps them, which then claims
-- them), which is the previous behaviour.
ALTER TABLE scope_members ADD CONSTRAINT scope_members_idp_issuer_source_check
    CHECK (source = 'idp' OR idp_issuer IS NULL);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tm_sync_idp_grants(p_principal uuid, p_issuer text, p_groups text[]) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF p_principal IS NULL OR p_principal IS DISTINCT FROM tm_principal() THEN
        RAISE EXCEPTION 'group sync runs as the signing-in principal' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- Remove grants this issuer owns (or unowned legacy IdP grants) that
    -- its mappings no longer justify. Other issuers' grants are untouched.
    DELETE FROM scope_members m
    WHERE m.principal_id = p_principal AND m.source = 'idp'
      AND (m.idp_issuer = p_issuer OR m.idp_issuer IS NULL)
      AND NOT EXISTS (SELECT 1 FROM idp_group_mappings g
                      WHERE g.issuer = p_issuer AND g.scope_id = m.scope_id
                        AND g.group_name = ANY (p_groups));
    -- Add or update the strongest mapped grant per scope. Manual grants
    -- win, and a grant owned by another issuer keeps its owner: when two
    -- providers map the same scope, the first grant stands until its own
    -- provider stops justifying it, and the other provider's mapping
    -- applies at its next sign-in after that.
    INSERT INTO scope_members (org_id, scope_id, principal_id, role, cascade, granted_by, source, idp_issuer)
    SELECT DISTINCT ON (g.scope_id) g.org_id, g.scope_id, p_principal, g.role, g.cascade, NULL, 'idp', p_issuer
    FROM idp_group_mappings g
    WHERE g.issuer = p_issuer AND g.group_name = ANY (p_groups)
    ORDER BY g.scope_id, tm_role_rank(g.role) DESC, g.cascade DESC
    ON CONFLICT (scope_id, principal_id) DO UPDATE
        SET role = EXCLUDED.role, cascade = EXCLUDED.cascade, idp_issuer = EXCLUDED.idp_issuer, updated_at = now()
        WHERE scope_members.source = 'idp'
          AND (scope_members.idp_issuer = EXCLUDED.idp_issuer OR scope_members.idp_issuer IS NULL)
          AND (scope_members.role, scope_members.cascade, scope_members.idp_issuer)
              IS DISTINCT FROM (EXCLUDED.role, EXCLUDED.cascade, EXCLUDED.idp_issuer);
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'migration 00005_idp_grant_issuer cannot be reversed'; END $$;
-- +goose StatementEnd
