-- +goose Up
-- +goose StatementBegin

-- Add a per-run monotonic CALL ORDINAL (seq) to steps and make it part of the
-- key.
--
-- The replay key was (run_id, step_name) with no ordinal, so a Step reused in a
-- loop resolved every iteration to the FIRST iteration's journal row: the side
-- effect ran once, iterations 2..N were served iteration 1's cached output, and
-- the run reported `succeeded`. A three-invoice loop sent one invoice and
-- reported success. Giving each iteration a distinct idempotency key did not
-- help, because the PRIMARY KEY (run_id, step_name, attempt) then collapsed all
-- of them onto a single row through RecordStepStart's ON CONFLICT DO NOTHING.
--
-- seq is 1-based. seq = 0 means "recorded by a workflow binary built against an
-- SDK that predates the ordinal". Those rows keep the legacy
-- (run_id, step_name) lookup, so customer workflows that are already compiled
-- and deployed keep replaying exactly as before; only rebuilt workflows get
-- ordinal semantics. That is what makes this migration safe to apply under
-- in-flight runs.
--
-- Keeping step_name IN the primary key is deliberate: for legacy rows (seq = 0)
-- the key degenerates to exactly the old (run_id, step_name, attempt), so the
-- previous uniqueness guarantee is preserved bit for bit.
--
-- SQLite cannot ALTER a PRIMARY KEY, so the table is rebuilt. Existing rows are
-- backfilled seq = 0 rather than a synthesised ordinal: inventing an ordering
-- for rows that were written without one would silently change how an in-flight
-- suspended run replays, which is the exact class of bug this migration exists
-- to remove.

ALTER TABLE steps RENAME TO steps_old;

CREATE TABLE steps (
    run_id          TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name       TEXT NOT NULL,
    seq             INTEGER NOT NULL DEFAULT 0,
    attempt         INTEGER NOT NULL,
    idempotency_key TEXT,
    input_hash      TEXT NOT NULL,
    output_jsonb    TEXT,
    error_text      TEXT,
    status          TEXT NOT NULL,        -- running | succeeded | failed | retrying
    started_at      TEXT NOT NULL,
    finished_at     TEXT,
    PRIMARY KEY (run_id, seq, step_name, attempt)
);

INSERT INTO steps (run_id, step_name, seq, attempt, idempotency_key, input_hash,
                   output_jsonb, error_text, status, started_at, finished_at)
SELECT run_id, step_name, 0, attempt, idempotency_key, input_hash,
       output_jsonb, error_text, status, started_at, finished_at
FROM steps_old;

DROP TABLE steps_old;

CREATE INDEX steps_run_status_idx ON steps(run_id, status);
CREATE INDEX steps_idempotency_idx ON steps(run_id, step_name, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX steps_run_seq_idx ON steps(run_id, seq);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Rebuild without seq. Ordinal rows collapse back onto the legacy key, so a
-- down-migration of a database that ran loop workflows can lose rows to the
-- restored (run_id, step_name, attempt) uniqueness; that is inherent to
-- reversing this change and is why the Up direction is the supported one.
ALTER TABLE steps RENAME TO steps_new;

CREATE TABLE steps (
    run_id          TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name       TEXT NOT NULL,
    attempt         INTEGER NOT NULL,
    idempotency_key TEXT,
    input_hash      TEXT NOT NULL,
    output_jsonb    TEXT,
    error_text      TEXT,
    status          TEXT NOT NULL,
    started_at      TEXT NOT NULL,
    finished_at     TEXT,
    PRIMARY KEY (run_id, step_name, attempt)
);

INSERT OR IGNORE INTO steps (run_id, step_name, attempt, idempotency_key, input_hash,
                             output_jsonb, error_text, status, started_at, finished_at)
SELECT run_id, step_name, attempt, idempotency_key, input_hash,
       output_jsonb, error_text, status, started_at, finished_at
FROM steps_new;

DROP TABLE steps_new;

CREATE INDEX steps_run_status_idx ON steps(run_id, status);
CREATE INDEX steps_idempotency_idx ON steps(run_id, step_name, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- +goose StatementEnd
