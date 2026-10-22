-- +goose Up
-- Stage 04: persistent OAuth authorization-server state, so restarts keep
-- refresh tokens and DCR clients and replicas share sign-ins in flight.
-- These are identity tables, not tenant-owned: no org_id, protected by
-- privileges rather than RLS. Codes, refresh tokens, login state values and
-- browser bindings are stored only as SHA-256 hashes. The upstream PKCE
-- verifier and nonce are stored as is for at most 10 minutes, because the
-- callback must send them to the upstream provider.

CREATE TABLE oauth_clients (
    id            text PRIMARY KEY CHECK (length(id) <= 100),
    name          text NOT NULL DEFAULT '' CHECK (length(name) <= 200),
    client_uri    text NOT NULL DEFAULT '' CHECK (length(client_uri) <= 2048),
    redirect_uris text[] NOT NULL CHECK (cardinality(redirect_uris) BETWEEN 1 AND 10),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- One authorization request from /oauth/authorize until its code is
-- redeemed (or it expires).
CREATE TABLE oauth_auth_requests (
    id                    uuid PRIMARY KEY,
    client_id             text NOT NULL,
    redirect_uri          text NOT NULL,
    scopes                text[] NOT NULL,
    state                 text NOT NULL DEFAULT '',
    nonce                 text NOT NULL DEFAULT '',
    response_type         text NOT NULL,
    response_mode         text NOT NULL DEFAULT '',
    code_challenge        text NOT NULL,
    code_challenge_method text NOT NULL,
    resource              text NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    subject               text,
    idp                   text,
    auth_time             timestamptz,
    done                  boolean NOT NULL DEFAULT false
);
CREATE INDEX oauth_auth_requests_created_idx ON oauth_auth_requests (created_at);

CREATE TABLE oauth_codes (
    code_hash  text PRIMARY KEY,
    request_id uuid NOT NULL REFERENCES oauth_auth_requests (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Rotating refresh tokens. used_at is set once, atomically, when a token is
-- exchanged; presenting it again revokes its family (reuse detection),
-- except inside a short grace window for concurrent client requests.
CREATE TABLE oauth_refresh_tokens (
    token_hash text PRIMARY KEY,
    family     uuid NOT NULL,
    client_id  text NOT NULL,
    subject    text NOT NULL,
    idp        text NOT NULL DEFAULT '',
    scopes     text[] NOT NULL,
    resource   text NOT NULL,
    auth_time  timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oauth_refresh_tokens_family_idx ON oauth_refresh_tokens (family);
CREATE INDEX oauth_refresh_tokens_expires_idx ON oauth_refresh_tokens (expires_at);

-- Upstream logins in flight, keyed by the hash of their state parameter and
-- bound to the starting browser by the hash of a cookie value.
CREATE TABLE oauth_pending_logins (
    state_hash      text PRIMARY KEY,
    auth_request_id uuid NOT NULL REFERENCES oauth_auth_requests (id) ON DELETE CASCADE,
    provider        text NOT NULL,
    nonce           text NOT NULL,
    verifier        text NOT NULL,
    browser_hash    text NOT NULL,
    expires_at      timestamptz NOT NULL
);
CREATE INDEX oauth_pending_logins_expires_idx ON oauth_pending_logins (expires_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON oauth_clients, oauth_auth_requests, oauth_codes,
    oauth_refresh_tokens, oauth_pending_logins TO team_memory_app, team_memory_operator;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'migration 00004_oauth_state cannot be reversed'; END $$;
-- +goose StatementEnd
