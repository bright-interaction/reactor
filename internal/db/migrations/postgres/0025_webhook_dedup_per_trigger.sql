-- +goose Up
-- +goose StatementBegin

-- Scope webhook delivery dedup to the receiving TRIGGER; see the sqlite mirror
-- for the full rationale. Short version: the key was (provider, delivery_id)
-- alone, a namespace shared by every trigger, and for the generic provider
-- delivery_id comes verbatim from the caller's X-Webhook-Delivery header. One
-- trigger consuming an id made every other trigger receiving that id answer
-- 200 {"deduped":true} and never dispatch. Live without any tenancy involved
-- (two workflows on the same GitHub org event), and a deliberate
-- denial-of-delivery across tenants.
--
-- Existing rows backfill trigger_id = '' rather than a guessed owner, so legacy
-- rows keep the old global namespace among themselves and age out through the
-- retention purge. Postgres alters the key in place, no table rebuild.

ALTER TABLE webhook_deliveries ADD COLUMN trigger_id TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_deliveries DROP CONSTRAINT webhook_deliveries_pkey;
ALTER TABLE webhook_deliveries ADD PRIMARY KEY (trigger_id, provider, delivery_id);
CREATE INDEX IF NOT EXISTS webhook_deliveries_received_idx ON webhook_deliveries(received_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS webhook_deliveries_received_idx;
DELETE FROM webhook_deliveries a USING webhook_deliveries b
  WHERE a.provider = b.provider AND a.delivery_id = b.delivery_id
    AND a.received_at > b.received_at;
ALTER TABLE webhook_deliveries DROP CONSTRAINT webhook_deliveries_pkey;
ALTER TABLE webhook_deliveries ADD PRIMARY KEY (provider, delivery_id);
ALTER TABLE webhook_deliveries DROP COLUMN trigger_id;
-- +goose StatementEnd
