-- +goose Up
-- +goose StatementBegin

-- Notification channels (Slack webhook, generic HTTPS webhook, email
-- via SMTP). One row per destination an operator wants to receive
-- run-status alerts on.
--
-- config_json carries the per-kind shape:
--   slack_webhook   -> {"url":"https://hooks.slack.com/..."}
--   generic_webhook -> {"url":"https://example.com/hook","headers":{"X-Foo":"bar"}}
--   email_smtp      -> {"host":"smtp.gmail.com","port":587,"username":"...","password_credential_id":"smtp-pass","from":"alerts@x.io","to":"ops@x.io"}
-- Sender code unmarshals into a typed struct so a bad value lands as
-- a runtime error, not silent acceptance.
CREATE TABLE notification_channels (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL CHECK (kind IN ('slack_webhook','generic_webhook','email_smtp')),
    config_json TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Per-workflow routing: which channel fires on which terminal status.
-- on_statuses is a comma-separated list of run statuses ('failed',
-- 'failed_dlq', 'succeeded'); the dispatcher's terminal hook splits +
-- compares.
CREATE TABLE workflow_notification_routes (
    workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    channel_id  TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    on_statuses TEXT NOT NULL DEFAULT 'failed,failed_dlq',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (workflow_id, channel_id)
);

CREATE INDEX workflow_notification_routes_workflow_idx
    ON workflow_notification_routes(workflow_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS workflow_notification_routes_workflow_idx;
DROP TABLE IF EXISTS workflow_notification_routes;
DROP TABLE IF EXISTS notification_channels;
-- +goose StatementEnd
