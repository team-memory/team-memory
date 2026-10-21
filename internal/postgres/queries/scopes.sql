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
              granted_by = EXCLUDED.granted_by, updated_at = now()
RETURNING *;

-- name: ListScopeMembers :many
SELECT m.*, p.display_name
FROM scope_members m JOIN principals p ON p.id = m.principal_id
WHERE m.scope_id = $1
ORDER BY p.display_name;

-- name: RolesOnScope :many
-- Roles that apply to a scope: a direct membership, or a cascading grant on an ancestor.
SELECT m.role
FROM scope_members m
JOIN scope_closure c ON c.ancestor_id = m.scope_id
WHERE c.descendant_id = @scope_id
  AND m.principal_id = @principal_id
  AND (c.distance = 0 OR m.cascade);

-- name: ReadableScopes :many
-- Scopes whose own memories the principal may read (direct membership or a
-- cascading ancestor grant). Ancestors of these are readable as inherited.
SELECT DISTINCT s.*
FROM scope_members m
JOIN scope_closure c ON c.ancestor_id = m.scope_id
JOIN scopes s ON s.id = c.descendant_id
WHERE m.principal_id = $1 AND (c.distance = 0 OR m.cascade)
ORDER BY s.path;

-- name: CanReadMemoriesOf :one
-- True when the principal can read the target scope directly or any descendant
-- of it (from which the target's memories are inherited).
SELECT EXISTS (
    SELECT 1
    FROM scope_members m
    JOIN scope_closure granted ON granted.ancestor_id = m.scope_id
    JOIN scope_closure inherit ON inherit.descendant_id = granted.descendant_id
    WHERE m.principal_id = @principal_id
      AND (granted.distance = 0 OR m.cascade)
      AND inherit.ancestor_id = @target_scope_id
)::boolean;

-- name: CountOrganizations :one
SELECT count(*)::integer FROM org_settings;
