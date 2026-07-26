-- +goose Up
-- +goose StatementBegin

-- Per-run call ordinal on schedules, the Sleep/AwaitSignal counterpart to
-- steps.seq (migration 0023).
--
-- Sleep and AwaitSignal both de-duplicated on (run_id, step_name, kind), so the
-- same missing-ordinal defect that made loops skip Steps also hit them:
--
--   * A Sleep inside a loop found the FIRST iteration's schedule row on every
--     later iteration. Its wake_at was already in the past, so the host acked
--     immediately and a per-item rate-limiting sleep degenerated to one sleep.
--
--   * Two AwaitSignal calls sharing a signal name collapsed onto ONE row, so the
--     second await returned the FIRST one's already-delivered payload without
--     ever suspending. An approve-then-confirm gate auto-confirmed itself from a
--     single approval click.
--
-- seq is 1-based; 0 means the row was written by a workflow binary built against
-- a pre-ordinal SDK and keeps the legacy (run_id, step_name, kind) lookup.
--
-- No primary key change is needed: schedules is keyed on its own id and the
-- lookups are ordinary queries, so a plain ADD COLUMN suffices (unlike steps,
-- whose composite key had to be rebuilt).

ALTER TABLE schedules ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
CREATE INDEX schedules_run_seq_idx ON schedules(run_id, seq, kind);

-- The signal token is derived from the signal NAME (SignalToken(name) is public
-- SDK API a workflow calls to build a URL BEFORE the await, so the ordinal is
-- not knowable at derivation time). Two AwaitSignal calls sharing a name
-- therefore legitimately share a token, and a UNIQUE index on signal_token
-- alone made the second await's INSERT fail outright once the ordinal stopped
-- collapsing them onto one row. Widen the uniqueness to (signal_token, seq):
-- accidental token reuse within one ordinal is still rejected, while successive
-- awaits of one signal name each get a row. FireSignal delivers to the oldest
-- row still awaiting a payload, so repeated deliveries fill them in order.
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
