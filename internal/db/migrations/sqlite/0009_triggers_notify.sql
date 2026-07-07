-- +goose Up
-- +goose StatementBegin

-- SQLite has no LISTEN/NOTIFY. The cron driver's polling reconcile loop
-- continues to be the only refresh path on SQLite. This empty migration
-- keeps the version numbering aligned between engines so a daemon that
-- migrates from sqlite to postgres later doesn't skip 0009 on the new
-- side.
SELECT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
