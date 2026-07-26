-- +goose Up
-- +goose StatementBegin

-- Per-run monotonic CALL ORDINAL on steps. See the sqlite mirror for the full
-- rationale: the replay key was (run_id, step_name) with no ordinal, so a Step
-- reused in a loop served every iteration the FIRST iteration's cached output,
-- ran the side effect once, and reported the run succeeded.
--
-- seq is 1-based; seq = 0 means the row was written by a workflow binary built
-- against a pre-ordinal SDK, and those keep legacy (run_id, step_name) lookup
-- semantics so already-compiled customer workflows are unaffected. Existing
-- rows are backfilled 0 by the column default rather than a synthesised
-- ordinal, which would change how an in-flight suspended run replays.
--
-- step_name stays in the primary key so that for legacy rows the key degenerates
-- to exactly the old (run_id, step_name, attempt).

ALTER TABLE steps ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE steps DROP CONSTRAINT steps_pkey;
ALTER TABLE steps ADD CONSTRAINT steps_pkey PRIMARY KEY (run_id, seq, step_name, attempt);
CREATE INDEX steps_run_seq_idx ON steps(run_id, seq);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reversing can violate the restored uniqueness on a database that ran loop
-- workflows (several ordinal rows share one (run_id, step_name, attempt)), so
-- collapse duplicates to the earliest row first.
DELETE FROM steps a USING steps b
 WHERE a.run_id = b.run_id
   AND a.step_name = b.step_name
   AND a.attempt = b.attempt
   AND a.seq > b.seq;

DROP INDEX IF EXISTS steps_run_seq_idx;
ALTER TABLE steps DROP CONSTRAINT steps_pkey;
ALTER TABLE steps ADD CONSTRAINT steps_pkey PRIMARY KEY (run_id, step_name, attempt);
ALTER TABLE steps DROP COLUMN seq;

-- +goose StatementEnd
