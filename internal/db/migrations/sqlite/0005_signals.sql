-- +goose Up
-- +goose StatementBegin

-- Signal delivery extends the schedules table. A row with kind='signal'
-- represents a workflow blocked at AwaitSignal: signal_token is the
-- public credential the external HTTP caller posts to, signal_payload
-- captures the body once delivered, and wake_at acts as the timeout
-- expiry that the existing FindDueSchedules sweep already enforces.
ALTER TABLE schedules ADD COLUMN signal_token   TEXT;
ALTER TABLE schedules ADD COLUMN signal_payload BLOB;

CREATE UNIQUE INDEX schedules_signal_token_idx ON schedules(signal_token)
    WHERE signal_token IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS schedules_signal_token_idx;
-- SQLite cannot DROP COLUMN before 3.35; the columns are additive and
-- unused on rollback so leaving them in place is safe.
-- +goose StatementEnd
