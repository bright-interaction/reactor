-- name: GetSchemaVersion :one
SELECT value FROM schema_meta WHERE key = 'schema_version';

-- name: GetEngineName :one
SELECT value FROM schema_meta WHERE key = 'engine';
