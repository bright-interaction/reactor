-- +goose Up
-- +goose StatementBegin

-- MFA phase 1. See the sqlite mirror for full design rationale. All factors
-- are per-user and opt-in; a user "has MFA" when they hold at least one
-- webauthn_credentials row OR a confirmed totp_secrets row.

-- `credential` holds the JSON of the library's canonical webauthn.Credential
-- (public key, sign count, backup flags, AAGUID, transports); see sqlite mirror.
CREATE TABLE webauthn_credentials (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id  TEXT NOT NULL UNIQUE,
    credential     TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at   TIMESTAMPTZ
);
CREATE INDEX webauthn_credentials_user_idx ON webauthn_credentials(user_id);

CREATE TABLE totp_secrets (
    user_id        TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret_enc     TEXT NOT NULL,
    algorithm      TEXT NOT NULL DEFAULT 'SHA1',
    digits         INTEGER NOT NULL DEFAULT 6,
    period         INTEGER NOT NULL DEFAULT 30,
    confirmed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at   TIMESTAMPTZ,
    last_used_step BIGINT
);

CREATE TABLE mfa_recovery_codes (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL UNIQUE,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mfa_recovery_codes_user_idx ON mfa_recovery_codes(user_id);

CREATE TABLE webauthn_challenges (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose       TEXT NOT NULL CHECK (purpose IN ('register','login','stepup')),
    session_data  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX webauthn_challenges_expires_idx ON webauthn_challenges(expires_at);

ALTER TABLE sessions ADD COLUMN mfa_pending BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE sessions ADD COLUMN elevated_until TIMESTAMPTZ;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS webauthn_challenges_expires_idx;
DROP TABLE IF EXISTS webauthn_challenges;
DROP INDEX IF EXISTS mfa_recovery_codes_user_idx;
DROP TABLE IF EXISTS mfa_recovery_codes;
DROP TABLE IF EXISTS totp_secrets;
DROP INDEX IF EXISTS webauthn_credentials_user_idx;
DROP TABLE IF EXISTS webauthn_credentials;
ALTER TABLE sessions DROP COLUMN IF EXISTS elevated_until;
ALTER TABLE sessions DROP COLUMN IF EXISTS mfa_pending;
-- +goose StatementEnd
