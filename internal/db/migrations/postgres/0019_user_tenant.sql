-- +goose Up
-- +goose StatementBegin

-- Associate each dashboard user with a tenant so the read views can be scoped:
-- members see only their tenant's workflows + runs, admins see everything.
ALTER TABLE users ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN tenant_id;
-- +goose StatementEnd
