-- +goose Up
-- +goose StatementBegin

-- Phase 3 (SaaS control plane): per-tenant fair scheduling, quotas, metering.
-- SQLite is local mode only; the fair claim + quotas matter in Postgres
-- (distributed) but the schema is mirrored so the journal code is identical.

ALTER TABLE runs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
UPDATE runs SET tenant_id = COALESCE((SELECT tenant_id FROM workflows WHERE workflows.id = runs.workflow_id), 'default');
CREATE INDEX runs_tenant_status_idx ON runs(tenant_id, status);

CREATE TABLE tenants (
    tenant_id            TEXT PRIMARY KEY,
    name                 TEXT NOT NULL DEFAULT '',
    plan                 TEXT NOT NULL DEFAULT 'free',
    max_concurrent_runs  INTEGER NOT NULL DEFAULT 0,
    max_queued_runs      INTEGER NOT NULL DEFAULT 0,
    monthly_run_quota    INTEGER NOT NULL DEFAULT 0,
    disabled             INTEGER NOT NULL DEFAULT 0,
    created_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
INSERT INTO tenants (tenant_id, name, plan) VALUES ('default', 'Default', 'unlimited');

CREATE TABLE run_usage (
    run_id       TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    workflow_id  TEXT NOT NULL,
    status       TEXT NOT NULL,
    run_seconds  REAL NOT NULL DEFAULT 0,
    step_count   INTEGER NOT NULL DEFAULT 0,
    started_at   TEXT,
    finished_at  TEXT,
    recorded_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX run_usage_tenant_time_idx ON run_usage(tenant_id, recorded_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS run_usage;
DROP TABLE IF EXISTS tenants;
DROP INDEX IF EXISTS runs_tenant_status_idx;
ALTER TABLE runs DROP COLUMN tenant_id;
-- +goose StatementEnd
