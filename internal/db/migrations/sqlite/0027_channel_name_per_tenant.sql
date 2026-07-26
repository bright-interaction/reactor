-- +goose Up
-- +goose StatementBegin

-- Make a channel name unique PER TENANT instead of globally.
--
-- 0026 gave notification_channels a tenant_id but left `name TEXT NOT NULL
-- UNIQUE` alone, so two tenants still could not both own a channel called
-- "ops-slack", and a failed create was a weak cross-tenant existence oracle
-- (try the name, read the conflict).
--
-- Narrowing it needs a table rebuild, and notification_channels is referenced by
-- workflow_notification_routes with ON DELETE CASCADE. A naive rebuild is what
-- destroyed credential_audit earlier in this series: SQLite rewrites the child's
-- REFERENCES to follow the RENAME, and the subsequent DROP then cascades the
-- child rows away.
--
-- The safe ordering, all inside goose's transaction with no PRAGMA toggling and
-- no reliance on foreign_keys being off:
--
--   1. copy the child rows aside into a real staging table
--   2. DROP the child, which REMOVES THE FK before the parent is touched
--   3. rebuild the parent with the composite unique
--   4. recreate the child, its FK now pointing at the new parent
--   5. restore the rows, then drop staging
--
-- Step 2 is the whole trick: with the child gone there is no reference to
-- follow and nothing for a cascade to delete. Row counts are asserted below so
-- a silent loss fails the migration instead of shipping.

CREATE TABLE _routes_staging AS SELECT * FROM workflow_notification_routes;

DROP INDEX IF EXISTS workflow_notification_routes_workflow_idx;
DROP TABLE workflow_notification_routes;

ALTER TABLE notification_channels RENAME TO _channels_old;

CREATE TABLE notification_channels (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('slack_webhook','generic_webhook','email_smtp')),
    config_json TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (tenant_id, name)
);

INSERT INTO notification_channels
    (id, tenant_id, name, kind, config_json, created_at, updated_at)
SELECT id, tenant_id, name, kind, config_json, created_at, updated_at
FROM _channels_old;

DROP TABLE _channels_old;

CREATE TABLE workflow_notification_routes (
    workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    channel_id  TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    on_statuses TEXT NOT NULL DEFAULT 'failed,failed_dlq',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (workflow_id, channel_id)
);

CREATE INDEX workflow_notification_routes_workflow_idx
    ON workflow_notification_routes(workflow_id);

INSERT INTO workflow_notification_routes
    (workflow_id, channel_id, on_statuses, created_at)
SELECT workflow_id, channel_id, on_statuses, created_at FROM _routes_staging;

-- Fail the migration rather than silently losing a route. RAISE() is
-- trigger-only in SQLite, so the portable assertion is a scratch table whose
-- CHECK cannot hold: a mismatch aborts the statement, goose rolls the
-- transaction back, and the database stays at 0026 instead of shipping short a
-- route.
CREATE TABLE _rebuild_assert (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO _rebuild_assert (ok) SELECT CASE
    WHEN (SELECT COUNT(*) FROM workflow_notification_routes)
       = (SELECT COUNT(*) FROM _routes_staging)
    THEN 1 ELSE 0
END;
DROP TABLE _rebuild_assert;

DROP TABLE _routes_staging;

CREATE INDEX notification_channels_tenant_idx ON notification_channels(tenant_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE _routes_staging AS SELECT * FROM workflow_notification_routes;
DROP INDEX IF EXISTS workflow_notification_routes_workflow_idx;
DROP INDEX IF EXISTS notification_channels_tenant_idx;
DROP TABLE workflow_notification_routes;
ALTER TABLE notification_channels RENAME TO _channels_new;
CREATE TABLE notification_channels (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    name        TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL CHECK (kind IN ('slack_webhook','generic_webhook','email_smtp')),
    config_json TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
-- Collapsing back to a global name can collide; keep the earliest per name.
INSERT INTO notification_channels
    (id, tenant_id, name, kind, config_json, created_at, updated_at)
SELECT id, tenant_id, name, kind, config_json, created_at, updated_at
FROM _channels_new WHERE id IN (SELECT MIN(id) FROM _channels_new GROUP BY name);
DROP TABLE _channels_new;
CREATE TABLE workflow_notification_routes (
    workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    channel_id  TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    on_statuses TEXT NOT NULL DEFAULT 'failed,failed_dlq',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (workflow_id, channel_id)
);
CREATE INDEX workflow_notification_routes_workflow_idx
    ON workflow_notification_routes(workflow_id);
-- Only routes whose channel survived the collapse can be restored.
INSERT INTO workflow_notification_routes
    (workflow_id, channel_id, on_statuses, created_at)
SELECT s.workflow_id, s.channel_id, s.on_statuses, s.created_at FROM _routes_staging s
WHERE s.channel_id IN (SELECT id FROM notification_channels);
DROP TABLE _routes_staging;
CREATE INDEX notification_channels_tenant_idx ON notification_channels(tenant_id);
-- +goose StatementEnd
