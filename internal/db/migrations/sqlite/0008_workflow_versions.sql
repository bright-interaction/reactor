-- +goose Up
-- +goose StatementBegin

-- Workflow version history. Each `reactor workflow register` (whether
-- from CLI, dashboard, or codegen prompt bar) inserts a new row with
-- the incoming sdk_version + code_hash + dag snapshot, and bumps the
-- version counter for the (workflow_id) pair. The existing `workflows`
-- row remains the current pointer (code_hash/sdk_version/dag_json
-- mirror the latest version) so old read paths keep working without
-- migration code; the version table is the audit trail and the future
-- "freeze runs to v3 while v4 lands" enforcement surface.
--
-- Replay-divergence detection in supervisor still fires on cache miss,
-- but with versions persisted an operator can also see exactly which
-- workflow-version a finished run executed against (the runs.workflow_version
-- column below pins each dispatched run to the version current at
-- dispatch time).
CREATE TABLE workflow_versions (
    workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    sdk_version TEXT NOT NULL,
    code_hash   TEXT NOT NULL,
    dag_json    TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (workflow_id, version)
);

CREATE INDEX workflow_versions_workflow_idx ON workflow_versions(workflow_id);

-- Pin each run to the workflow version current at dispatch time so the
-- audit trail tells the truth even after the workflow gets re-registered
-- with new code. Nullable to keep pre-0008 rows valid; new rows always
-- carry the dispatched version.
ALTER TABLE runs ADD COLUMN workflow_version INTEGER;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS workflow_versions_workflow_idx;
DROP TABLE IF EXISTS workflow_versions;
-- SQLite doesn't support DROP COLUMN reliably across versions; the
-- runs.workflow_version column stays nullable so leaving it in place
-- after a down-migration is harmless.
-- +goose StatementEnd
