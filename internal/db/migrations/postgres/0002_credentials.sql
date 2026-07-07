-- +goose Up
-- +goose StatementBegin

CREATE TABLE credentials (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    name            TEXT NOT NULL,
    service         TEXT NOT NULL,
    rotation_policy TEXT NOT NULL DEFAULT 'auto',
    blob            BYTEA NOT NULL,
    upstream_handle TEXT,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at      TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE INDEX credentials_service_idx
    ON credentials(service)
    WHERE deleted_at IS NULL;

CREATE INDEX credentials_expires_idx
    ON credentials(expires_at)
    WHERE deleted_at IS NULL;

CREATE TABLE credential_audit (
    id            BIGSERIAL PRIMARY KEY,
    credential_id TEXT NOT NULL REFERENCES credentials(id),
    action        TEXT NOT NULL,
    actor_kind    TEXT NOT NULL,
    actor_id      TEXT,
    workflow_id   TEXT,
    run_id        TEXT,
    step_id       TEXT,
    detail        JSONB NOT NULL DEFAULT '{}'::jsonb,
    at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX credential_audit_cred_at_idx
    ON credential_audit(credential_id, at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS credential_audit_cred_at_idx;
DROP TABLE IF EXISTS credential_audit;
DROP INDEX IF EXISTS credentials_expires_idx;
DROP INDEX IF EXISTS credentials_service_idx;
DROP TABLE IF EXISTS credentials;
-- +goose StatementEnd
