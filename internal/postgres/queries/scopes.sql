-- name: CreateRootScope :one
-- The trigger derives org_id, depth and path; org_id here must equal id.
INSERT INTO scopes (id, org_id, parent_id, level, slug, name, depth, path, created_by)
VALUES (@id, @id, NULL, @level, @slug, @name, 0, @slug, @created_by)
RETURNING *;

-- name: CreateChildScope :one
-- org_id, depth and path are placeholders overwritten by the trigger.
INSERT INTO scopes (org_id, parent_id, level, slug, name, depth, path, created_by)
SELECT p.org_id, p.id, @level, @slug, @name, p.depth + 1, p.path || '/' || @slug::text, @created_by
FROM scopes p WHERE p.id = @parent_id
RETURNING *;

-- name: GetScopeByID :one
SELECT * FROM scopes WHERE id = $1;

-- name: GetScopeByPath :one
SELECT * FROM scopes WHERE path = $1;

-- name: ListScopesInOrg :many
SELECT * FROM scopes WHERE org_id = $1 ORDER BY path;

-- name: ListOrgRoots :many
SELECT * FROM scopes WHERE parent_id IS NULL ORDER BY path;

-- name: ListAncestors :many
-- The scope itself (distance 0) followed by its ancestors, nearest first.
SELECT s.*, c.distance
FROM scope_closure c JOIN scopes s ON s.id = c.ancestor_id
WHERE c.descendant_id = $1
ORDER BY c.distance;

-- name: InsertOrgSettings :one
INSERT INTO org_settings (org_id, allowed_levels, max_depth, owner_principal_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetOrgSettings :one
SELECT * FROM org_settings WHERE org_id = $1;

-- name: LockOrgSettings :one
SELECT * FROM org_settings WHERE org_id = $1 FOR UPDATE;

-- name: UpdateOrgSettings :one
UPDATE org_settings
SET allowed_levels = $2, max_depth = $3, updated_at = now()
WHERE org_id = $1
RETURNING *;

-- name: MaxScopeDepthInOrg :one
SELECT coalesce(max(depth), 0)::integer FROM scopes WHERE org_id = $1;

-- name: UpsertScopeMember :one
INSERT INTO scope_members (org_id, scope_id, principal_id, role, cascade, granted_by)
SELECT s.org_id, s.id, @principal_id, @role, @cascade, @granted_by FROM scopes s WHERE s.id = @scope_id
ON CONFLICT (scope_id, principal_id)
DO UPDATE SET role = EXCLUDED.role, cascade = EXCLUDED.cascade,
              granted_by = EXCLUDED.granted_by, source = 'manual', idp_issuer = NULL, updated_at = now()
RETURNING *;

-- name: ListScopeMembers :many
SELECT m.*, p.display_name
FROM scope_members m JOIN principals p ON p.id = m.principal_id
WHERE m.scope_id = $1
ORDER BY p.display_name;

-- name: AccessOnScope :one
-- The principal's precomputed access to a scope (scope_access): rank 1..3 is
-- the effective role (direct membership or a cascading ancestor grant),
-- rank 0 means readable only as inherited. No row means no access.
SELECT rank, cascade_rank FROM scope_access
WHERE principal_id = @principal_id AND scope_id = @scope_id;

-- name: ReadableScopes :many
-- Scopes whose own memories the principal may read (direct membership or a
-- cascading ancestor grant). Ancestors of these are readable as inherited.
SELECT s.*, a.rank
FROM scope_access a
JOIN scopes s ON s.id = a.scope_id
WHERE a.principal_id = $1 AND a.rank >= 1
ORDER BY s.path;

-- name: CanReadMemoriesOf :one
-- True when the principal can read the target scope directly or any descendant
-- of it (from which the target's memories are inherited).
SELECT EXISTS (
    SELECT 1 FROM scope_access
    WHERE principal_id = @principal_id AND scope_id = @target_scope_id
)::boolean;

-- name: InsertChildScope :exec
-- Principal path: no RETURNING, because the new scope becomes readable only
-- when the access trigger runs at the end of the statement.
INSERT INTO scopes (id, org_id, parent_id, level, slug, name, depth, path, created_by)
SELECT @id, p.org_id, p.id, @level, @slug, @name, p.depth + 1, p.path || '/' || @slug::text, @created_by
FROM scopes p WHERE p.id = @parent_id;

-- name: GetScopeMember :one
SELECT * FROM scope_members WHERE scope_id = @scope_id AND principal_id = @principal_id;

-- name: DeleteScopeMember :execrows
DELETE FROM scope_members WHERE scope_id = @scope_id AND principal_id = @principal_id;

-- name: ListMembershipsOfPrincipal :many
SELECT m.*, s.path AS scope_path
FROM scope_members m JOIN scopes s ON s.id = m.scope_id
WHERE m.principal_id = $1
ORDER BY s.path;

-- name: InsertIdPGroupMapping :exec
INSERT INTO idp_group_mappings (org_id, issuer, group_name, scope_id, role, cascade)
SELECT s.org_id, @issuer, @group_name, s.id, @role, @cascade FROM scopes s WHERE s.id = @scope_id
ON CONFLICT (issuer, group_name, scope_id) DO UPDATE SET role = EXCLUDED.role, cascade = EXCLUDED.cascade;

-- name: DeleteIdPGroupMapping :execrows
DELETE FROM idp_group_mappings WHERE issuer = @issuer AND group_name = @group_name AND scope_id = @scope_id;

-- name: ListIdPGroupMappings :many
SELECT g.*, s.path AS scope_path
FROM idp_group_mappings g JOIN scopes s ON s.id = g.scope_id
ORDER BY g.issuer, g.group_name, s.path;

-- name: SyncIdPGrants :exec
SELECT tm_sync_idp_grants(@principal_id::uuid, @issuer::text, @groups::text[]);

-- name: CountOrganizations :one
SELECT count(*)::integer FROM org_settings;
