-- +goose Up
-- +goose StatementBegin

-- Workflow version history. See sqlite mirror for the design rationale.
CREATE TABLE workflow_versions (
    workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    sdk_version TEXT NOT NULL,
    code_hash   TEXT NOT NULL,
    dag_json    JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, version)
);

CREATE INDEX workflow_versions_workflow_idx ON workflow_versions(workflow_id);

ALTER TABLE runs ADD COLUMN workflow_version INTEGER;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS workflow_versions_workflow_idx;
DROP TABLE IF EXISTS workflow_versions;
ALTER TABLE runs DROP COLUMN IF EXISTS workflow_version;
-- +goose StatementEnd
