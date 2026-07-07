-- +goose Up
-- +goose StatementBegin

-- Operator-declared "manual baseline" for each workflow: how many
-- minutes a person would have spent doing this run by hand. Drives the
-- "time saved" rollup on the home dashboard:
--
--   total_minutes_saved = SUM(estimated_minutes_saved_per_run * succeeded_runs)
--
-- 0 (the default) means the operator has not declared a baseline yet
-- and the workflow contributes 0 to the time-saved total. The field is
-- editable per workflow via the dashboard's workflow detail page.
ALTER TABLE workflows ADD COLUMN estimated_minutes_saved_per_run INTEGER NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- SQLite doesn't support DROP COLUMN reliably across versions; leave
-- the column in place on a down-migration. It defaults to 0 so old
-- code paths that ignore the field are unaffected.
-- +goose StatementEnd
