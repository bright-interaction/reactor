-- +goose Up
-- +goose StatementBegin

-- See sqlite mirror for design rationale.
CREATE TABLE users (
    id             TEXT PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE,
    password_phc   TEXT NOT NULL,
    role           TEXT NOT NULL CHECK (role IN ('admin', 'member')) DEFAULT 'member',
    disabled       BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at  TIMESTAMPTZ
);

CREATE TABLE sessions (
    id_hash       TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_agent    TEXT,
    ip            TEXT
);
CREATE INDEX sessions_user_idx ON sessions(user_id);
CREATE INDEX sessions_expires_idx ON sessions(expires_at);

CREATE TABLE api_tokens (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    token_hash    TEXT NOT NULL UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    revoked       BOOLEAN NOT NULL DEFAULT false
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
