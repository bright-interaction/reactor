-- +goose Up
-- +goose StatementBegin

ALTER TABLE workflows ADD COLUMN rate_limit_per_min INTEGER NOT NULL DEFAULT 0;
CREATE INDEX runs_workflow_created_idx ON runs(workflow_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS runs_workflow_created_idx;
ALTER TABLE workflows DROP COLUMN rate_limit_per_min;
-- +goose StatementEnd
