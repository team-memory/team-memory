-- name: CreatePrincipal :one
INSERT INTO principals (kind, display_name) VALUES ($1, $2) RETURNING *;

-- name: GetPrincipal :one
SELECT * FROM principals WHERE id = $1;

-- name: ListPrincipals :many
SELECT * FROM principals ORDER BY created_at;

-- name: InsertPersonalAccessToken :one
INSERT INTO personal_access_tokens (principal_id, name, token_prefix, token_hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, principal_id, name, token_prefix, created_at, expires_at;

-- name: LookupPersonalAccessToken :one
SELECT t.id, t.principal_id, t.expires_at, t.revoked_at, p.kind, p.display_name, p.disabled_at
FROM personal_access_tokens t
JOIN principals p ON p.id = t.principal_id
WHERE t.token_hash = $1;

-- name: TouchPersonalAccessToken :exec
UPDATE personal_access_tokens SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: RevokePersonalAccessToken :execrows
UPDATE personal_access_tokens SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- name: ListPersonalAccessTokens :many
SELECT id, principal_id, name, token_prefix, created_at, expires_at, revoked_at, last_used_at
FROM personal_access_tokens
WHERE principal_id = $1
ORDER BY created_at;

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (org_id, actor_id, action, target_type, target_id, scope_id, outcome, details)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetPrincipalByIdentity :one
SELECT p.* FROM principal_identities i
JOIN principals p ON p.id = i.principal_id
WHERE i.issuer = $1 AND i.subject = $2;

-- name: InsertPrincipalIdentity :execrows
INSERT INTO principal_identities (principal_id, issuer, subject)
VALUES ($1, $2, $3)
ON CONFLICT (issuer, subject) DO NOTHING;

-- name: ListPrincipalIdentities :many
SELECT * FROM principal_identities WHERE principal_id = $1 ORDER BY created_at;
