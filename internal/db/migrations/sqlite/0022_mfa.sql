-- +goose Up
-- +goose StatementBegin

-- MFA phase 1: WebAuthn passkeys, TOTP, one-time recovery codes, and the
-- session-level assurance columns that gate MFA-at-login and step-up "sudo".
-- All factors are per-user and opt-in; a user "has MFA" when they hold at
-- least one webauthn_credentials row OR a confirmed totp_secrets row.

-- Registered WebAuthn passkeys (platform authenticators / security keys).
-- Stores only PUBLIC key material; there is no server-side secret to leak.
-- The `credential` column is the JSON of the library's canonical
-- webauthn.Credential (public key, sign count, backup-eligible/backup-state
-- flags, AAGUID, transports). Keeping the whole record as the library emits
-- it is the recommended storage shape and means a library update cannot
-- silently drop a security-relevant field via a hand-mapped column.
CREATE TABLE webauthn_credentials (
    id             TEXT PRIMARY KEY,                        -- wac_<hex>
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id  TEXT NOT NULL UNIQUE,                    -- base64url(raw credential id); allowed-credential lookup
    credential     TEXT NOT NULL,                           -- JSON of webauthn.Credential (source of truth)
    name           TEXT NOT NULL DEFAULT '',                -- operator label ("MacBook Touch ID")
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_used_at   TEXT
);
CREATE INDEX webauthn_credentials_user_idx ON webauthn_credentials(user_id);

-- Per-user TOTP enrollment. secret_enc is the AES-256-GCM ciphertext of the
-- base32 shared secret (never plaintext; a DB snapshot does not yield the
-- second factor). confirmed_at stays NULL until the user proves a code, so a
-- half-finished enrollment is not a live factor. last_used_step records the
-- last accepted time-step counter to block same-window code replay.
CREATE TABLE totp_secrets (
    user_id        TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret_enc     TEXT NOT NULL,                              -- base64(nonce||ciphertext||tag)
    algorithm      TEXT NOT NULL DEFAULT 'SHA1',
    digits         INTEGER NOT NULL DEFAULT 6,
    period         INTEGER NOT NULL DEFAULT 30,
    confirmed_at   TEXT,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_used_at   TEXT,
    last_used_step INTEGER
);

-- One-time MFA recovery codes. Codes are high-entropy; stored as sha256 hex
-- (fast hash is fine at this entropy and matches api_tokens.token_hash).
-- used_at is set on consumption so a code works exactly once and the audit
-- trail of which code was spent survives.
CREATE TABLE mfa_recovery_codes (
    id          TEXT PRIMARY KEY,                              -- rec_<hex>
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL UNIQUE,
    used_at     TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX mfa_recovery_codes_user_idx ON mfa_recovery_codes(user_id);

-- Ephemeral WebAuthn ceremony state (the challenge + expected params) held
-- between begin and finish. Keyed by an opaque id handed to the browser and
-- swept on expiry. purpose pins the flow so a challenge minted to register a
-- key cannot be replayed to satisfy a login or a step-up.
CREATE TABLE webauthn_challenges (
    id            TEXT PRIMARY KEY,                            -- wch_<hex>
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose       TEXT NOT NULL CHECK (purpose IN ('register','login','stepup')),
    session_data  TEXT NOT NULL,                               -- JSON webauthn.SessionData
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    expires_at    TEXT NOT NULL
);
CREATE INDEX webauthn_challenges_expires_idx ON webauthn_challenges(expires_at);

-- Session assurance level. mfa_pending = password accepted but the second
-- factor is still owed (the session grants nothing but the MFA + logout
-- routes until it clears). elevated_until = step-up "sudo" window minted by a
-- fresh factor assertion, required for dangerous actions.
ALTER TABLE sessions ADD COLUMN mfa_pending INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN elevated_until TEXT;

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
ALTER TABLE sessions DROP COLUMN elevated_until;
ALTER TABLE sessions DROP COLUMN mfa_pending;
-- +goose StatementEnd
