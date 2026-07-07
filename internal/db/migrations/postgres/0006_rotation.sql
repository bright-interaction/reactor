-- +goose Up
-- +goose StatementBegin

ALTER TABLE credentials ADD COLUMN provider               TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE credentials ADD COLUMN provider_meta          JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE credentials ADD COLUMN auto_rotate            BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE credentials ADD COLUMN rotation_interval_days INTEGER NOT NULL DEFAULT 90;
ALTER TABLE credentials ADD COLUMN last_rotated_at        TIMESTAMPTZ;
ALTER TABLE credentials ADD COLUMN last_rotation_error    TEXT;
ALTER TABLE credentials ADD COLUMN rotation_targets       JSONB NOT NULL DEFAULT '[]'::jsonb;

CREATE INDEX credentials_auto_rotate_idx
    ON credentials(auto_rotate, last_rotated_at)
    WHERE deleted_at IS NULL AND auto_rotate = true;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS credentials_auto_rotate_idx;
ALTER TABLE credentials DROP COLUMN IF EXISTS rotation_targets;
ALTER TABLE credentials DROP COLUMN IF EXISTS last_rotation_error;
ALTER TABLE credentials DROP COLUMN IF EXISTS last_rotated_at;
ALTER TABLE credentials DROP COLUMN IF EXISTS rotation_interval_days;
ALTER TABLE credentials DROP COLUMN IF EXISTS auto_rotate;
ALTER TABLE credentials DROP COLUMN IF EXISTS provider_meta;
ALTER TABLE credentials DROP COLUMN IF EXISTS provider;
-- +goose StatementEnd
