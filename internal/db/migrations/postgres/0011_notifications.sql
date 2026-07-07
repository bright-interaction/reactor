-- +goose Up
-- +goose StatementBegin

-- See sqlite mirror for design rationale.
CREATE TABLE notification_channels (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL CHECK (kind IN ('slack_webhook','generic_webhook','email_smtp')),
    config_json JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workflow_notification_routes (
    workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    channel_id  TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    on_statuses TEXT NOT NULL DEFAULT 'failed,failed_dlq',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
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
