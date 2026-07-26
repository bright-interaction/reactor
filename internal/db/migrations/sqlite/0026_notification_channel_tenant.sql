-- +goose Up
-- +goose StatementBegin

-- Give notification_channels a tenant owner.
--
-- It is one of only two tables with NO tenant anchor at all: everything else
-- reaches a tenant through run_id or workflow_id, but a channel is a top-level
-- entity reachable only by id (webhook_deliveries was the other, fixed in 0025
-- by keying on trigger_id). So ListNotificationChannels returned every
-- channel in the install to every caller.
--
-- The visible consequence: /notifications is admin-gated, but the workflow
-- detail page is MEMBER-facing and renders an "attach channel" <select> built
-- from that same unscoped list. A member browsing their own workflow could read
-- back the id, name and kind of every alert channel an admin had configured,
-- and post-tenancy every other tenant's too. Config JSON was never rendered, so
-- no webhook URL or SMTP password leaked, and the attach POST is admin-gated so
-- they could not act on it. A metadata leak, not an escalation.
--
-- Plain ADD COLUMN, deliberately: notification_channels is referenced by
-- workflow_notification_routes with ON DELETE CASCADE, so rebuilding it would
-- fire that cascade and take the routes with it. That is exactly how an earlier
-- attempt in this series destroyed credential_audit. Existing rows take
-- 'default', which matches every other resource's pre-tenancy owner.
--
-- NOT changed here: `name TEXT NOT NULL UNIQUE` stays GLOBALLY unique, so two
-- tenants still cannot both own a channel called "ops-slack", and a failed
-- create is a weak existence oracle across tenants. Narrowing it to
-- (tenant_id, name) needs the table rebuild this migration is avoiding, so it
-- wants its own FK-safe pass.

ALTER TABLE notification_channels ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
CREATE INDEX notification_channels_tenant_idx ON notification_channels(tenant_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS notification_channels_tenant_idx;
ALTER TABLE notification_channels DROP COLUMN tenant_id;
-- +goose StatementEnd
