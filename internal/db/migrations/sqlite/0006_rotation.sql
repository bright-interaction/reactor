-- +goose Up
-- +goose StatementBegin

-- Rotation state columns on the existing credentials table. The audit
-- log uses the existing credential_audit table from migration 0002.
-- A row is "due for rotation" when:
--   auto_rotate = 1
--   AND deleted_at IS NULL
--   AND (last_rotated_at IS NULL OR last_rotated_at + rotation_interval_days < now)
ALTER TABLE credentials ADD COLUMN provider              TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE credentials ADD COLUMN provider_meta         TEXT NOT NULL DEFAULT '{}';
ALTER TABLE credentials ADD COLUMN auto_rotate           INTEGER NOT NULL DEFAULT 0;
ALTER TABLE credentials ADD COLUMN rotation_interval_days INTEGER NOT NULL DEFAULT 90;
ALTER TABLE credentials ADD COLUMN last_rotated_at       TEXT;
ALTER TABLE credentials ADD COLUMN last_rotation_error   TEXT;
ALTER TABLE credentials ADD COLUMN rotation_targets      TEXT NOT NULL DEFAULT '[]';

CREATE INDEX credentials_auto_rotate_idx
    ON credentials(auto_rotate, last_rotated_at)
    WHERE deleted_at IS NULL AND auto_rotate = 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS credentials_auto_rotate_idx;
-- SQLite < 3.35 can't DROP COLUMN; columns are additive and unused on rollback.
-- +goose StatementEnd
