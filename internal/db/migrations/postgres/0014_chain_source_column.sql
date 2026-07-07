-- +goose Up
-- +goose StatementBegin

-- See sqlite mirror for rationale. config_json is JSONB here.
ALTER TABLE triggers ADD COLUMN source_workflow_id TEXT;

UPDATE triggers
SET source_workflow_id = config_json->>'source_workflow_id'
WHERE kind = 'workflow_complete';

CREATE INDEX triggers_source_workflow_idx
    ON triggers(source_workflow_id)
    WHERE source_workflow_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS triggers_source_workflow_idx;
ALTER TABLE triggers DROP COLUMN source_workflow_id;
-- +goose StatementEnd
