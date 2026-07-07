-- +goose Up
-- +goose StatementBegin

-- Worker heartbeat registry for distributed mode. Each `reactor worker`
-- upserts its row periodically; the dashboard reads it to show the live
-- fleet, and the autoscaler reads active capacity (workers whose last_seen
-- is recent) to decide whether to add or remove workers. Stale rows (dead
-- workers) are pruned by last_seen.
CREATE TABLE workers (
    worker_id   TEXT PRIMARY KEY,
    concurrency INTEGER NOT NULL,
    last_seen   TEXT NOT NULL,
    started_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX workers_last_seen_idx ON workers(last_seen);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS workers;
-- +goose StatementEnd
