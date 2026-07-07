// Package db owns the embedded migrations FS so the binary ships with its schema.
// Engine-specific subdirectories (migrations/sqlite, migrations/postgres) keep
// each dialect's SQL idiomatic.
package db

import "embed"

//go:embed all:migrations/sqlite all:migrations/postgres
var Migrations embed.FS
