-- +goose Up
-- +goose StatementBegin

-- Per-step journal. The host writes one row per (run, step, attempt) tuple
-- at step_start (status=running) and updates it at step_end (status=
-- succeeded|failed). On host restart the workflow subprocess re-spawns
-- and replays Run() from the top; every Step call hits this table to
-- resolve "should I re-execute or return cached output?".
--
-- PK is (run_id, step_name, attempt). Idempotency keys live in the
-- separate idempotency_key column so retries within one run dedup
-- without storing duplicate attempt rows.
CREATE TABLE steps (
    run_id          TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name       TEXT NOT NULL,
    attempt         INTEGER NOT NULL,
    idempotency_key TEXT,
    input_hash      TEXT NOT NULL,
    output_jsonb    TEXT,
    error_text      TEXT,
    status          TEXT NOT NULL,        -- running | succeeded | failed | retrying
    started_at      TEXT NOT NULL,
    finished_at     TEXT,
    PRIMARY KEY (run_id, step_name, attempt)
);
CREATE INDEX steps_run_status_idx ON steps(run_id, status);
CREATE INDEX steps_idempotency_idx ON steps(run_id, step_name, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Worker leases. A run can be picked up by exactly one supervisor
-- goroutine at a time. Leases expire so a crashed supervisor releases
-- the run for re-spawn by the next scheduler tick.
CREATE TABLE leases (
    run_id     TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    worker_id  TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX leases_expires_idx ON leases(expires_at);

-- Schedules. Sleep / AwaitSignal / cron all land here. The scheduler
-- selects WHERE wake_at <= now AND fired = 0 and dispatches.
CREATE TABLE schedules (
    id           TEXT PRIMARY KEY,
    run_id       TEXT REFERENCES runs(id) ON DELETE CASCADE,
    step_name    TEXT,
    kind         TEXT NOT NULL,           -- sleep | signal | cron
    wake_at      TEXT,
    signal_name  TEXT,
    fired        INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX schedules_wake_idx ON schedules(wake_at) WHERE fired = 0;

-- Webhook delivery dedup. Inbound webhooks check (provider, delivery_id)
-- before queueing a run; replays inside the configured window are 200 no-ops.
CREATE TABLE webhook_deliveries (
    provider     TEXT NOT NULL,
    delivery_id  TEXT NOT NULL,
    received_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (provider, delivery_id)
);

-- Dead-letter queue. Steps that exhausted retries land here for manual
-- inspection / replay via `reactor replay`.
CREATE TABLE dead_letter (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES runs(id),
    step_name   TEXT NOT NULL,
    error_text  TEXT NOT NULL,
    payload     TEXT NOT NULL,
    moved_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
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
