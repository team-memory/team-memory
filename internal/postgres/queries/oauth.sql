-- name: OAuthInsertClient :exec
INSERT INTO oauth_clients (id, name, client_uri, redirect_uris) VALUES ($1, $2, $3, $4);

-- name: OAuthGetClient :one
SELECT * FROM oauth_clients WHERE id = $1;

-- name: OAuthCountClients :one
SELECT count(*)::integer FROM oauth_clients;

-- name: OAuthInsertAuthRequest :exec
INSERT INTO oauth_auth_requests (id, client_id, redirect_uri, scopes, state, nonce, response_type, response_mode,
                                 code_challenge, code_challenge_method, resource, created_at)
VALUES (@id, @client_id, @redirect_uri, @scopes, @state, @nonce, @response_type, @response_mode,
        @code_challenge, @code_challenge_method, @resource, @created_at);

-- name: OAuthCountAuthRequests :one
SELECT count(*)::integer FROM oauth_auth_requests WHERE created_at > @since;

-- name: OAuthGetAuthRequest :one
SELECT * FROM oauth_auth_requests WHERE id = @id AND created_at > @since;

-- name: OAuthCompleteAuthRequest :execrows
UPDATE oauth_auth_requests SET subject = @subject, idp = @idp, auth_time = @auth_time, done = true
WHERE id = @id AND NOT done AND created_at > @since;

-- name: OAuthDeleteAuthRequest :exec
DELETE FROM oauth_auth_requests WHERE id = $1;

-- name: OAuthInsertCode :exec
INSERT INTO oauth_codes (code_hash, request_id) VALUES ($1, $2);

-- name: OAuthTakeCode :one
-- Single use: the row is deleted as it is read.
DELETE FROM oauth_codes WHERE code_hash = $1 RETURNING request_id;

-- name: OAuthInsertRefreshToken :exec
INSERT INTO oauth_refresh_tokens (token_hash, family, client_id, subject, idp, scopes, resource, auth_time, expires_at)
VALUES (@token_hash, @family, @client_id, @subject, @idp, @scopes, @resource, @auth_time, @expires_at);

-- name: OAuthGetRefreshToken :one
SELECT * FROM oauth_refresh_tokens WHERE token_hash = $1;

-- name: OAuthUseRefreshToken :one
-- Marks a token used exactly once. Concurrent exchanges of the same token
-- race on this row; only one gets it back.
UPDATE oauth_refresh_tokens SET used_at = @now
WHERE token_hash = @token_hash AND used_at IS NULL AND expires_at > @now
RETURNING *;

-- name: OAuthRevokeRefreshFamily :exec
DELETE FROM oauth_refresh_tokens WHERE family = $1;

-- name: OAuthInsertPendingLogin :exec
INSERT INTO oauth_pending_logins (state_hash, auth_request_id, provider, nonce, verifier, browser_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: OAuthCountPendingLogins :one
SELECT count(*)::integer FROM oauth_pending_logins WHERE expires_at > @now;

-- name: OAuthTakePendingLogin :one
DELETE FROM oauth_pending_logins WHERE state_hash = $1 RETURNING *;

-- name: OAuthSweepAuthRequests :exec
-- Expired requests; their codes and pending logins cascade.
DELETE FROM oauth_auth_requests WHERE created_at < $1;

-- name: OAuthSweepRefreshTokens :exec
DELETE FROM oauth_refresh_tokens WHERE expires_at < $1;

-- name: OAuthSweepPendingLogins :exec
DELETE FROM oauth_pending_logins WHERE expires_at < $1;
