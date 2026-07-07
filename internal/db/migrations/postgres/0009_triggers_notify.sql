-- +goose Up
-- +goose StatementBegin

-- Notify listeners (the cron driver's live-reload subscriber) whenever
-- the triggers table changes so cron-kind reconciliation can happen
-- immediately instead of waiting for the polling tick. SQLite has no
-- equivalent; the polling reconcile remains the only path there.
CREATE OR REPLACE FUNCTION reactor_notify_triggers_changed() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('reactor_triggers_changed', COALESCE(NEW.id::text, OLD.id::text));
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER reactor_triggers_notify
    AFTER INSERT OR UPDATE OR DELETE ON triggers
    FOR EACH ROW EXECUTE FUNCTION reactor_notify_triggers_changed();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS reactor_triggers_notify ON triggers;
DROP FUNCTION IF EXISTS reactor_notify_triggers_changed();
-- +goose StatementEnd
