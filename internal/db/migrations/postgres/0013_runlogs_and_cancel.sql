-- +goose Up
-- +goose StatementBegin

-- See sqlite mirror for design rationale.
CREATE TABLE run_logs (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    line       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

ALTER TABLE runs ADD COLUMN cancel_requested BOOLEAN NOT NULL DEFAULT false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS run_logs;
ALTER TABLE runs DROP COLUMN cancel_requested;
-- +goose StatementEnd
