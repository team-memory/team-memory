-- +goose Up
-- Stage 05: web portal sessions. These are identity tables, not tenant-owned
-- (no org_id): protected by privileges, like personal_access_tokens. The
-- session id and the login state value are stored only as SHA-256 hashes, so
-- a database read cannot be replayed as a cookie. The CSRF token is bound to
-- one session and useless without its cookie.

CREATE TABLE portal_sessions (
    id_hash      text PRIMARY KEY,
    principal_id uuid NOT NULL REFERENCES principals (id),
    csrf_token   text NOT NULL,
    -- how the person signed in: token (PAT) or sso
    method       text NOT NULL CHECK (method IN ('token', 'sso')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz
);
CREATE INDEX portal_sessions_principal_idx ON portal_sessions (principal_id);
CREATE INDEX portal_sessions_expires_idx ON portal_sessions (expires_at);

-- Upstream logins started by the portal (sign-in or Connect SSO). The
-- verifier and nonce are kept as is for at most 10 minutes, because the
-- callback must send them upstream. A link login is bound to the session
-- that started it.
CREATE TABLE portal_login_states (
    state_hash   text PRIMARY KEY,
    provider     text NOT NULL,
    purpose      text NOT NULL CHECK (purpose IN ('login', 'link')),
    nonce        text NOT NULL,
    verifier     text NOT NULL,
    browser_hash text NOT NULL,
    session_hash text,
    expires_at   timestamptz NOT NULL
);
CREATE INDEX portal_login_states_expires_idx ON portal_login_states (expires_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON portal_sessions, portal_login_states TO team_memory_app, team_memory_operator;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'migration 00006_portal cannot be reversed'; END $$;
-- +goose StatementEnd
