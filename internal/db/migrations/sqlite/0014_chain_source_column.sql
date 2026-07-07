-- +goose Up
-- +goose StatementBegin

-- Promote a chain trigger's source workflow id from inside config_json to
-- an indexed column. ChainTriggersForSource runs on every terminal run;
-- previously it scanned every workflow_complete trigger and parsed the
-- source out of JSON in Go. With the column + index the lookup is a
-- direct indexed read.
ALTER TABLE triggers ADD COLUMN source_workflow_id TEXT;

UPDATE triggers
SET source_workflow_id = json_extract(config_json, '$.source_workflow_id')
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
