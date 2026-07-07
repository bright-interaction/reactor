-- +goose Up
-- +goose StatementBegin

-- Persistent run logs. The in-process ring buffer (internal/runlogs) is
-- flushed here when a run terminates, so "last night's failure" still has
-- its full log tail after the 10-minute in-memory grace window expires.
-- seq orders the lines within a run; (run_id, seq) is the natural key.
CREATE TABLE run_logs (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    line       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (run_id, seq)
);

-- Cross-process run cancellation. The CLI and MCP run in separate
-- processes from the daemon, so they request cancellation by setting this
-- flag; the daemon's cancel watcher observes it and kills the live
-- subprocess. The dashboard (in-daemon) also cancels in-process for
-- instant feedback. Suspended runs are cancelled directly (no live
-- process to signal).
ALTER TABLE runs ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS run_logs;
ALTER TABLE runs DROP COLUMN cancel_requested;
-- +goose StatementEnd
