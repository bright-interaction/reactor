-- +goose Up
-- +goose StatementBegin

-- Trigger registry. One row per (workflow, kind) binding. Webhook
-- triggers carry a public token + an HMAC credential reference; cron
-- carries a robfig/cron spec; cdc + docker land in later weeks.
--
-- The token_id column is the URL-routable opaque identifier for webhook
-- bindings: POST /webhook/{token_id} resolves to (workflow_id, secret_id)
-- without exposing the workflow's slug or the underlying credential ID.
CREATE TABLE triggers (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL DEFAULT 'default',
    workflow_id   TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,                 -- webhook | cron | cdc | docker | manual
    config_json   TEXT NOT NULL DEFAULT '{}',
    state         TEXT NOT NULL DEFAULT 'active',
    token_id      TEXT,                          -- webhook public token; null for non-webhook
    secret_id     TEXT,                          -- vault credential ID for HMAC; null for non-webhook
    provider      TEXT,                          -- stripe | github | generic; webhook-only
    last_fired_at TEXT,
    last_error    TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
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
