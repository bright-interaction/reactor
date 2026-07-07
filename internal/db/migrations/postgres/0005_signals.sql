-- +goose Up
-- +goose StatementBegin

ALTER TABLE schedules ADD COLUMN signal_token   TEXT;
ALTER TABLE schedules ADD COLUMN signal_payload BYTEA;

CREATE UNIQUE INDEX schedules_signal_token_idx ON schedules(signal_token)
    WHERE signal_token IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS schedules_signal_token_idx;
ALTER TABLE schedules DROP COLUMN IF EXISTS signal_payload;
ALTER TABLE schedules DROP COLUMN IF EXISTS signal_token;
-- +goose StatementEnd
