-- +goose Up
-- +goose StatementBegin

-- Billing plans (tiers). A plan bundles the operational caps (max_concurrent,
-- max_queued) with the billing allotments (included executions + compute) and
-- overage rates. Assigning a plan to a tenant copies these onto the tenant so
-- the existing claim + enqueue enforcement is unchanged. Money is in integer
-- minor units (cents); 0 included/overage = unlimited / not billed.
CREATE TABLE plans (
    plan_id                       TEXT PRIMARY KEY,
    name                          TEXT NOT NULL DEFAULT '',
    price_cents                   INTEGER NOT NULL DEFAULT 0,
    currency                      TEXT NOT NULL DEFAULT 'EUR',
    included_executions           INTEGER NOT NULL DEFAULT 0,
    included_compute_seconds      BIGINT  NOT NULL DEFAULT 0,
    max_concurrent_runs           INTEGER NOT NULL DEFAULT 0,
    max_queued_runs               INTEGER NOT NULL DEFAULT 0,
    overage_per_1k_exec_cents     INTEGER NOT NULL DEFAULT 0,
    overage_per_compute_hour_cents INTEGER NOT NULL DEFAULT 0,
    -- hard_cap true: refuse runs past the included allotment (no surprise
    -- bill, for free/unverified tenants). false: allow + bill overage.
    hard_cap                      BOOLEAN NOT NULL DEFAULT true,
    sort_order                    INTEGER NOT NULL DEFAULT 0,
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A tenant's plan + the cap mode copied from it. plan_id NULL = custom quotas
-- set by hand (no plan).
ALTER TABLE tenants ADD COLUMN plan_id  TEXT REFERENCES plans(plan_id) ON DELETE SET NULL;
ALTER TABLE tenants ADD COLUMN hard_cap BOOLEAN NOT NULL DEFAULT true;

-- Seed starter tiers (illustrative EUR; edit on the Plans page). Free hard-caps
-- at its included volume; paid tiers bill overage past it.
INSERT INTO plans (plan_id, name, price_cents, included_executions, included_compute_seconds, max_concurrent_runs, max_queued_runs, overage_per_1k_exec_cents, overage_per_compute_hour_cents, hard_cap, sort_order) VALUES
    ('free',    'Free',     0,      1000,    6000,    2,  50,    0,   0,   true,  0),
    ('starter', 'Starter',  2900,   10000,   60000,   5,  500,   200, 50,  false, 1),
    ('pro',     'Pro',      9900,   100000,  600000,  20, 2000,  150, 40,  false, 2),
    ('scale',   'Scale',    39900,  500000,  3000000, 50, 10000, 100, 30,  false, 3);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenants DROP COLUMN hard_cap;
ALTER TABLE tenants DROP COLUMN plan_id;
DROP TABLE IF EXISTS plans;
-- +goose StatementEnd
