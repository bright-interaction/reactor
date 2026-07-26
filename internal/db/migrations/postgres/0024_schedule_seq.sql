-- +goose Up
-- +goose StatementBegin

-- Per-run call ordinal on schedules; see the sqlite mirror for full rationale.
-- Sleep and AwaitSignal de-duplicated on (run_id, step_name, kind), so a Sleep
-- in a loop fired once and two AwaitSignal calls sharing a name collapsed onto
-- one row, letting the second await return the first's already-delivered payload
-- without suspending. seq is 1-based; 0 keeps legacy semantics for workflow
-- binaries built before the ordinal.

ALTER TABLE schedules ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
CREATE INDEX schedules_run_seq_idx ON schedules(run_id, seq, kind);

-- See the sqlite mirror: the signal token is name-derived (SignalToken(name) is
-- public SDK API called BEFORE the await), so two awaits of one name share a
-- token and a UNIQUE index on signal_token alone rejected the second await's
-- row once the ordinal stopped collapsing them. Widen to (signal_token, seq).
DROP INDEX IF EXISTS schedules_signal_token_idx;
CREATE UNIQUE INDEX schedules_signal_token_idx ON schedules(signal_token, seq)
    WHERE signal_token IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS schedules_signal_token_idx;
DROP INDEX IF EXISTS schedules_run_seq_idx;
ALTER TABLE schedules DROP COLUMN seq;
CREATE UNIQUE INDEX schedules_signal_token_idx ON schedules(signal_token)
    WHERE signal_token IS NOT NULL;
-- +goose StatementEnd
