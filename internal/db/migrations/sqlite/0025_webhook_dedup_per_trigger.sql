-- +goose Up
-- +goose StatementBegin

-- Scope webhook delivery dedup to the receiving TRIGGER.
--
-- The key was (provider, delivery_id) alone, a namespace shared by every
-- trigger in the install. provider collapses to three values, and for the
-- generic provider delivery_id is taken verbatim from the caller's
-- X-Webhook-Delivery header. So one trigger could consume an id and any OTHER
-- trigger receiving the same id was answered 200 {"deduped":true} and never
-- dispatched.
--
-- This is live without any tenancy involved: two workflows subscribed to the
-- same GitHub org event get one delivery id fanned out to both URLs, the first
-- to arrive claims it, and the second workflow silently never runs. Across
-- tenants it is a deliberate denial-of-delivery: post to your own trigger
-- carrying the victim's delivery id and their automation stops, with the
-- receiver reporting success to their upstream the whole time.
--
-- trigger_id also gives the row a tenant anchor for free (triggers.workflow_id
-- -> workflows.tenant_id), which is why this table needed no separate
-- tenant_id column.
--
-- Existing rows backfill trigger_id = '' rather than a guessed owner: a
-- historical delivery cannot be attributed to a trigger after the fact, and
-- inventing one would change dedup behaviour for in-flight retries. Legacy rows
-- therefore keep the old global namespace among themselves and age out through
-- PurgeOldWebhookDeliveries, which is the same "legacy rows keep legacy
-- semantics" shape used for steps.seq in 0023.
--
-- The rebuild is safe here, unlike the credentials one that had to be reverted:
-- webhook_deliveries has no foreign key in EITHER direction, so nothing follows
-- the rename and the DROP takes no child rows with it. Verified by grep before
-- writing this.

ALTER TABLE webhook_deliveries RENAME TO webhook_deliveries_old;

CREATE TABLE webhook_deliveries (
    trigger_id   TEXT NOT NULL DEFAULT '',
    provider     TEXT NOT NULL,
    delivery_id  TEXT NOT NULL,
    received_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (trigger_id, provider, delivery_id)
);

INSERT INTO webhook_deliveries (trigger_id, provider, delivery_id, received_at)
SELECT '', provider, delivery_id, received_at FROM webhook_deliveries_old;

DROP TABLE webhook_deliveries_old;

-- The purge sweeps by age, so keep that path indexed.
CREATE INDEX webhook_deliveries_received_idx ON webhook_deliveries(received_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS webhook_deliveries_received_idx;
ALTER TABLE webhook_deliveries RENAME TO webhook_deliveries_new;
CREATE TABLE webhook_deliveries (
    provider     TEXT NOT NULL,
    delivery_id  TEXT NOT NULL,
    received_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (provider, delivery_id)
);
-- Collapsing back can hit duplicates that were legal under the wider key, so
-- keep the earliest row per (provider, delivery_id).
INSERT INTO webhook_deliveries (provider, delivery_id, received_at)
SELECT provider, delivery_id, MIN(received_at) FROM webhook_deliveries_new
GROUP BY provider, delivery_id;
DROP TABLE webhook_deliveries_new;
-- +goose StatementEnd
