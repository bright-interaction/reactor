-- +goose Up
-- +goose StatementBegin

-- Phase 3 (SaaS control plane): per-tenant fair scheduling, quotas, metering.

-- Denormalize the owning tenant onto runs so the fair-share claim, quota
-- counts, and usage rollups never have to join workflows on the hot path.
ALTER TABLE runs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
UPDATE runs SET tenant_id = w.tenant_id FROM workflows w WHERE w.id = runs.workflow_id;
CREATE INDEX runs_tenant_status_idx ON runs(tenant_id, status);

-- Tenant registry + quota config. A quota of 0 means unlimited.
CREATE TABLE tenants (
    tenant_id            TEXT PRIMARY KEY,
    name                 TEXT NOT NULL DEFAULT '',
    plan                 TEXT NOT NULL DEFAULT 'free',
    max_concurrent_runs  INTEGER NOT NULL DEFAULT 0,
    max_queued_runs      INTEGER NOT NULL DEFAULT 0,
    monthly_run_quota    INTEGER NOT NULL DEFAULT 0,
    disabled             BOOLEAN NOT NULL DEFAULT false,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO tenants (tenant_id, name, plan) VALUES ('default', 'Default', 'unlimited');

-- Per-run usage ledger for metering/billing. One row per run, written when
-- the run reaches a terminal status; aggregated per tenant per period.
CREATE TABLE run_usage (
    run_id       TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    workflow_id  TEXT NOT NULL,
    status       TEXT NOT NULL,
    run_seconds  DOUBLE PRECISION NOT NULL DEFAULT 0,
    step_count   INTEGER NOT NULL DEFAULT 0,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    recorded_at  TIMESTAMPTZ NOT NULL DEFAULT now()
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
