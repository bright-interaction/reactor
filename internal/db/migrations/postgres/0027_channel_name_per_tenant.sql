-- +goose Up
-- +goose StatementBegin

-- Make a channel name unique PER TENANT instead of globally. See the sqlite
-- mirror for the rationale; on postgres this is a constraint swap rather than a
-- table rebuild, so the workflow_notification_routes FK is never disturbed and
-- there is no cascade to dodge.
--
-- The old constraint is dropped by DISCOVERY rather than by its expected name
-- (notification_channels_name_key). A hardcoded name plus DROP CONSTRAINT IF
-- EXISTS fails SILENTLY when the name differs for any reason: the migration
-- reports success, the global UNIQUE survives, and the tenancy fix looks shipped
-- while two tenants still cannot share a name. So find every single-column
-- unique on `name` and drop what is actually there.

DO $$
DECLARE c text;
BEGIN
    FOR c IN
        SELECT con.conname
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        JOIN pg_attribute att ON att.attrelid = rel.oid AND att.attnum = con.conkey[1]
        WHERE rel.relname = 'notification_channels'
          AND con.contype = 'u'
          AND array_length(con.conkey, 1) = 1
          AND att.attname = 'name'
    LOOP
        EXECUTE format('ALTER TABLE notification_channels DROP CONSTRAINT %I', c);
    END LOOP;
END $$;

ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_tenant_name_key
    UNIQUE (tenant_id, name);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE notification_channels DROP CONSTRAINT IF EXISTS notification_channels_tenant_name_key;
-- Collapsing back to a global name can collide; drop the later duplicates so the
-- constraint can be re-added. Routes cascade with them, which is the same
-- information loss the sqlite mirror documents.
DELETE FROM notification_channels WHERE id NOT IN (SELECT MIN(id) FROM notification_channels GROUP BY name);
ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_name_key UNIQUE (name);
-- +goose StatementEnd
