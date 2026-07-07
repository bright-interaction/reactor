-- +goose Up
-- +goose StatementBegin

-- Billing plans (tiers). See the postgres mirror for the column rationale.
CREATE TABLE plans (
    plan_id                       TEXT PRIMARY KEY,
    name                          TEXT NOT NULL DEFAULT '',
    price_cents                   INTEGER NOT NULL DEFAULT 0,
    currency                      TEXT NOT NULL DEFAULT 'EUR',
    included_executions           INTEGER NOT NULL DEFAULT 0,
    included_compute_seconds      INTEGER NOT NULL DEFAULT 0,
    max_concurrent_runs           INTEGER NOT NULL DEFAULT 0,
    max_queued_runs               INTEGER NOT NULL DEFAULT 0,
    overage_per_1k_exec_cents     INTEGER NOT NULL DEFAULT 0,
    overage_per_compute_hour_cents INTEGER NOT NULL DEFAULT 0,
    hard_cap                      INTEGER NOT NULL DEFAULT 1,
    sort_order                    INTEGER NOT NULL DEFAULT 0,
    created_at                    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at                    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

ALTER TABLE tenants ADD COLUMN plan_id  TEXT REFERENCES plans(plan_id) ON DELETE SET NULL;
ALTER TABLE tenants ADD COLUMN hard_cap INTEGER NOT NULL DEFAULT 1;

INSERT INTO plans (plan_id, name, price_cents, included_executions, included_compute_seconds, max_concurrent_runs, max_queued_runs, overage_per_1k_exec_cents, overage_per_compute_hour_cents, hard_cap, sort_order) VALUES
    ('free',    'Free',     0,      1000,    6000,    2,  50,    0,   0,   1, 0),
    ('starter', 'Starter',  2900,   10000,   60000,   5,  500,   200, 50,  0, 1),
    ('pro',     'Pro',      9900,   100000,  600000,  20, 2000,  150, 40,  0, 2),
    ('scale',   'Scale',    39900,  500000,  3000000, 50, 10000, 100, 30,  0, 3);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenants DROP COLUMN hard_cap;
ALTER TABLE tenants DROP COLUMN plan_id;
DROP TABLE IF EXISTS plans;
-- +goose StatementEnd
