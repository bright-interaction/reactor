-- +goose Up
-- +goose StatementBegin

CREATE TABLE triggers (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL DEFAULT 'default',
    workflow_id   TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    config_json   JSONB NOT NULL DEFAULT '{}'::jsonb,
    state         TEXT NOT NULL DEFAULT 'active',
    token_id      TEXT,
    secret_id     TEXT,
    provider      TEXT,
    last_fired_at TIMESTAMPTZ,
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX triggers_token_idx ON triggers(token_id) WHERE token_id IS NOT NULL;
CREATE INDEX triggers_workflow_idx ON triggers(workflow_id);
CREATE INDEX triggers_kind_state_idx ON triggers(kind, state);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS triggers_kind_state_idx;
DROP INDEX IF EXISTS triggers_workflow_idx;
DROP INDEX IF EXISTS triggers_token_idx;
DROP TABLE IF EXISTS triggers;
-- +goose StatementEnd
