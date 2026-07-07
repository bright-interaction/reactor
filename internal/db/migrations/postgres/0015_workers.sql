-- +goose Up
-- +goose StatementBegin

-- See sqlite mirror for rationale.
CREATE TABLE workers (
    worker_id   TEXT PRIMARY KEY,
    concurrency INTEGER NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX workers_last_seen_idx ON workers(last_seen);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS workers;
-- +goose StatementEnd
