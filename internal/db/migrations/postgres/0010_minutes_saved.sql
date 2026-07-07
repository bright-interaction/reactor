-- +goose Up
-- +goose StatementBegin

-- See sqlite mirror for design rationale.
ALTER TABLE workflows ADD COLUMN estimated_minutes_saved_per_run INTEGER NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflows DROP COLUMN IF EXISTS estimated_minutes_saved_per_run;
-- +goose StatementEnd
