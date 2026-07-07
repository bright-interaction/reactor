-- +goose Up
-- +goose StatementBegin

-- Health check / schema version sentinel.
-- Proves migrations ran end to end on this engine.
CREATE TABLE schema_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

INSERT INTO schema_meta (key, value) VALUES ('schema_version', '1');
INSERT INTO schema_meta (key, value) VALUES ('engine', 'sqlite');

-- Workflow definitions. Code lives on disk under reactor-workflows/<slug>/;
-- this row is the index + run-history join point.
CREATE TABLE workflows (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    slug         TEXT NOT NULL,
    git_sha      TEXT,
    code_hash    TEXT NOT NULL,
    sdk_version  TEXT NOT NULL,
    dag_json     TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (tenant_id, slug)
);

CREATE TABLE runs (
    id            TEXT PRIMARY KEY,
    workflow_id   TEXT NOT NULL REFERENCES workflows(id),
    trigger_kind  TEXT NOT NULL,
    trigger_meta  TEXT NOT NULL,
    status        TEXT NOT NULL,
    started_at    TEXT,
    finished_at   TEXT,
    parent_run_id TEXT REFERENCES runs(id),
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX runs_status_started ON runs(status, started_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS runs_status_started;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS workflows;
DROP TABLE IF EXISTS schema_meta;
-- +goose StatementEnd
