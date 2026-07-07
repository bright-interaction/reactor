-- +goose Up
-- +goose StatementBegin

CREATE TABLE steps (
    run_id          TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name       TEXT NOT NULL,
    attempt         INT NOT NULL,
    idempotency_key TEXT,
    input_hash      TEXT NOT NULL,
    output_jsonb    JSONB,
    error_text      TEXT,
    status          TEXT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ,
    PRIMARY KEY (run_id, step_name, attempt)
);
CREATE INDEX steps_run_status_idx ON steps(run_id, status);
CREATE INDEX steps_idempotency_idx ON steps(run_id, step_name, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE leases (
    run_id     TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    worker_id  TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX leases_expires_idx ON leases(expires_at);

CREATE TABLE schedules (
    id           TEXT PRIMARY KEY,
    run_id       TEXT REFERENCES runs(id) ON DELETE CASCADE,
    step_name    TEXT,
    kind         TEXT NOT NULL,
    wake_at      TIMESTAMPTZ,
    signal_name  TEXT,
    fired        BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX schedules_wake_idx ON schedules(wake_at) WHERE fired = false;

CREATE TABLE webhook_deliveries (
    provider     TEXT NOT NULL,
    delivery_id  TEXT NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, delivery_id)
);

CREATE TABLE dead_letter (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES runs(id),
    step_name   TEXT NOT NULL,
    error_text  TEXT NOT NULL,
    payload     JSONB NOT NULL,
    moved_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dead_letter;
DROP TABLE IF EXISTS webhook_deliveries;
DROP INDEX IF EXISTS schedules_wake_idx;
DROP TABLE IF EXISTS schedules;
DROP INDEX IF EXISTS leases_expires_idx;
DROP TABLE IF EXISTS leases;
DROP INDEX IF EXISTS steps_idempotency_idx;
DROP INDEX IF EXISTS steps_run_status_idx;
DROP TABLE IF EXISTS steps;
-- +goose StatementEnd
