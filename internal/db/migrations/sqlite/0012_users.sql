-- +goose Up
-- +goose StatementBegin

-- Per-operator user identity. Replaces the single shared BasicAuth
-- account with a real user table; existing BasicAuth setups keep
-- working because the BasicAuth middleware still resolves against the
-- legacy env-var-configured credentials when no users exist yet
-- (single-tenant compatibility floor; goes away in F3 phase 2 when
-- teams ship). password_phc is the argon2id PHC string EncodeArgon2id
-- PHC produces (Pass A); legacy hex-sha256 strings also accepted by
-- newPasswordVerifier so an operator can paste an env var value here
-- as the first admin without re-hashing.
CREATE TABLE users (
    id             TEXT PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE,
    password_phc   TEXT NOT NULL,
    role           TEXT NOT NULL CHECK (role IN ('admin', 'member')) DEFAULT 'member',
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_login_at  TEXT
);

-- Browser sessions. id is the cookie value the operator's browser
-- sends back; the column is hashed (sha256 hex) so a database snapshot
-- does not yield session theft. expires_at is a hard deadline; the
-- middleware checks expires_at > now on every request. Sessions
-- persist across daemon restarts so a graceful shutdown does not log
-- everyone out.
CREATE TABLE sessions (
    id_hash       TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    expires_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    user_agent    TEXT,
    ip            TEXT
);
CREATE INDEX sessions_user_idx ON sessions(user_id);
CREATE INDEX sessions_expires_idx ON sessions(expires_at);

-- Personal access tokens for programmatic clients (CI, scripts,
-- third-party integrations). The user sees the raw token once at
-- mint time; the column stores the sha256 hex hash so a leak
-- elsewhere does not expose live tokens. last_used_at lets the
-- operator audit which tokens are still hot.
CREATE TABLE api_tokens (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    token_hash    TEXT NOT NULL UNIQUE,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_used_at  TEXT,
    expires_at    TEXT,
    revoked       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX api_tokens_user_idx ON api_tokens(user_id);
CREATE INDEX api_tokens_revoked_idx ON api_tokens(revoked);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS api_tokens_revoked_idx;
DROP INDEX IF EXISTS api_tokens_user_idx;
DROP INDEX IF EXISTS sessions_expires_idx;
DROP INDEX IF EXISTS sessions_user_idx;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
