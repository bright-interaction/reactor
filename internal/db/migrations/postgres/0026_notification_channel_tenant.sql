-- +goose Up
-- +goose StatementBegin

-- Give notification_channels a tenant owner; see the sqlite mirror for the full
-- rationale. Short version: it is a top-level entity reachable only by id, so it
-- had no tenant anchor and ListNotificationChannels returned every channel in
-- the install. The member-facing workflow detail page renders an attach-channel
-- <select> from that unscoped list, leaking the id/name/kind of every channel
-- (config JSON was never rendered, and the attach POST is admin-gated, so it is
-- a metadata leak rather than an escalation).
--
-- Plain ADD COLUMN: workflow_notification_routes references this table with
-- ON DELETE CASCADE, so a rebuild would take the routes with it.
--
-- NOT changed here: name stays globally UNIQUE, so two tenants cannot both own
-- "ops-slack". Narrowing that to (tenant_id, name) wants its own pass.

ALTER TABLE notification_channels ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
CREATE INDEX notification_channels_tenant_idx ON notification_channels(tenant_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS notification_channels_tenant_idx;
ALTER TABLE notification_channels DROP COLUMN tenant_id;
-- +goose StatementEnd
