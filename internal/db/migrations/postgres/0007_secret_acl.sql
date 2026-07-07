-- +goose Up
-- +goose StatementBegin

CREATE TABLE workflow_secret_grants (
    workflow_id   TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    granted_by    TEXT,
    note          TEXT,
    PRIMARY KEY (workflow_id, credential_id)
);

CREATE INDEX workflow_secret_grants_cred_idx
    ON workflow_secret_grants(credential_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS workflow_secret_grants_cred_idx;
DROP TABLE IF EXISTS workflow_secret_grants;
-- +goose StatementEnd
