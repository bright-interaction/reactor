-- +goose Up
-- +goose StatementBegin

-- Per-workflow secret ACL. Without this row, a workflow's vault.MustGet
-- call against a credential id is REJECTED at the supervisor's
-- handleSecretFetch boundary. Existing installs migrate cleanly because
-- the supervisor's check is strict-when-table-non-empty: an empty
-- workflow_secret_grants table preserves the v0 contract (any workflow
-- can read any credential), so dropping in 0007 + adding rows turns
-- on per-workflow scoping incrementally.
--
-- Composite primary key on (workflow_id, credential_id) makes the
-- existence check a single indexed lookup at every SecretFetch frame.
CREATE TABLE workflow_secret_grants (
    workflow_id   TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    granted_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    granted_by    TEXT,
    note          TEXT,
    PRIMARY KEY (workflow_id, credential_id)
);

-- Lookup-by-credential index supports "which workflows can read this
-- credential?" queries from the future dashboard / MCP surfaces.
CREATE INDEX workflow_secret_grants_cred_idx
    ON workflow_secret_grants(credential_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS workflow_secret_grants_cred_idx;
DROP TABLE IF EXISTS workflow_secret_grants;
-- +goose StatementEnd
