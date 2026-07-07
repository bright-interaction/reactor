-- +goose Up
-- +goose StatementBegin

-- Billable compute is ACTIVE execution time, not wall clock. A run that sleeps
-- 7 days (suspend-and-resume: the subprocess exits, zero compute) must not be
-- billed for the idle wait. run_seconds stays as a wall-clock diagnostic;
-- active_seconds (sum of step durations, which excludes suspended time) is the
-- billable compute meter.
ALTER TABLE run_usage ADD COLUMN active_seconds DOUBLE PRECISION NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE run_usage DROP COLUMN active_seconds;
-- +goose StatementEnd
