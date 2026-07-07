-- +goose Up
-- +goose StatementBegin

-- OAuth2 connections (sqlite mirror; BLOB for encrypted bytes, INTEGER for
-- bool, TEXT ISO timestamps). See the postgres mirror for the rationale.
CREATE TABLE oauth_providers (
    provider_id             TEXT PRIMARY KEY,
    name                    TEXT NOT NULL DEFAULT '',
    auth_url                TEXT NOT NULL,
    token_url               TEXT NOT NULL,
    client_id               TEXT NOT NULL DEFAULT '',
    client_secret_encrypted BLOB,
    scopes                  TEXT NOT NULL DEFAULT '',
    enabled                 INTEGER NOT NULL DEFAULT 0,
    created_at              TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at              TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE oauth_connections (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    provider_id     TEXT NOT NULL REFERENCES oauth_providers(provider_id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    token_encrypted BLOB NOT NULL,
    expires_at      TEXT,
    scopes          TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'connected',
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (tenant_id, provider_id, name)
);
CREATE INDEX oauth_connections_tenant_idx ON oauth_connections(tenant_id);

CREATE TABLE oauth_states (
    state         TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    provider_id   TEXT NOT NULL,
    name          TEXT NOT NULL,
    redirect_uri  TEXT NOT NULL,
    code_verifier TEXT NOT NULL DEFAULT '',
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS oauth_connections;
DROP TABLE IF EXISTS oauth_providers;
-- +goose StatementEnd
