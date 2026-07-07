-- +goose Up
-- +goose StatementBegin

-- OAuth2 connections: let workflows act as a connected third-party account
-- (Google, Slack, GitHub, ...) without the user pasting raw API keys. Provider
-- client credentials + per-connection tokens are encrypted at rest with the
-- master key (same envelope encryption as the vault).

-- Provider catalog (operator-configured client app per provider).
CREATE TABLE oauth_providers (
    provider_id             TEXT PRIMARY KEY,        -- 'google', 'slack', 'github'
    name                    TEXT NOT NULL DEFAULT '',
    auth_url                TEXT NOT NULL,
    token_url               TEXT NOT NULL,
    client_id               TEXT NOT NULL DEFAULT '',
    client_secret_encrypted BYTEA,                   -- master-key encrypted
    scopes                  TEXT NOT NULL DEFAULT '', -- space-separated defaults
    enabled                 BOOLEAN NOT NULL DEFAULT false,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A connected account, scoped to a tenant. The token blob (access + refresh +
-- expiry + scopes) is encrypted; expires_at is kept in plaintext so the
-- refresh decision needs no decrypt.
CREATE TABLE oauth_connections (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    provider_id     TEXT NOT NULL REFERENCES oauth_providers(provider_id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    token_encrypted BYTEA NOT NULL,
    expires_at      TIMESTAMPTZ,
    scopes          TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'connected', -- connected | error | revoked
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, provider_id, name)
);
CREATE INDEX oauth_connections_tenant_idx ON oauth_connections(tenant_id);

-- Transient CSRF/PKCE state between the authorize redirect and the callback.
CREATE TABLE oauth_states (
    state         TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    provider_id   TEXT NOT NULL,
    name          TEXT NOT NULL,
    redirect_uri  TEXT NOT NULL,
    code_verifier TEXT NOT NULL DEFAULT '',
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS oauth_connections;
DROP TABLE IF EXISTS oauth_providers;
-- +goose StatementEnd
