-- +goose Up
-- +goose StatementBegin

-- Per-workflow rate limit: cap how many runs a single workflow may start per
-- minute (0 = unlimited). Enforced at dispatch by counting recent runs, so it
-- is correct across multiple serve/worker instances (no in-memory drift).
ALTER TABLE workflows ADD COLUMN rate_limit_per_min INTEGER NOT NULL DEFAULT 0;

-- Supports the count-in-last-minute probe + the per-workflow run list.
CREATE INDEX runs_workflow_created_idx ON runs(workflow_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS runs_workflow_created_idx;
ALTER TABLE workflows DROP COLUMN rate_limit_per_min;
-- +goose StatementEnd
