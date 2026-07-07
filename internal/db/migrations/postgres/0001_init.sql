-- +goose Up
-- +goose StatementBegin

CREATE TABLE schema_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO schema_meta (key, value) VALUES ('schema_version', '1');
INSERT INTO schema_meta (key, value) VALUES ('engine', 'postgres');

CREATE TABLE workflows (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    slug         TEXT NOT NULL,
    git_sha      TEXT,
    code_hash    TEXT NOT NULL,
    sdk_version  TEXT NOT NULL,
    dag_json     JSONB NOT NULL,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, slug)
);

CREATE TABLE runs (
    id            TEXT PRIMARY KEY,
    workflow_id   TEXT NOT NULL REFERENCES workflows(id),
    trigger_kind  TEXT NOT NULL,
    trigger_meta  JSONB NOT NULL,
    status        TEXT NOT NULL,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    parent_run_id TEXT REFERENCES runs(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
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
