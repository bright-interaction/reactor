package migrate

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestEngineFromURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		url     string
		want    Engine
		wantErr bool
	}{
		{"sqlite scheme", "sqlite://./test.db", EngineSQLite, false},
		{"sqlite3 scheme", "sqlite3://./test.db", EngineSQLite, false},
		{"file scheme", "file:./test.db", EngineSQLite, false},
		{"postgres scheme", "postgres://u:p@h/d", EnginePostgres, false},
		{"postgresql scheme", "postgresql://u:p@h/d", EnginePostgres, false},
		{"empty url", "", "", true},
		{"unsupported scheme", "mysql://u:p@h/d", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EngineFromURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got engine %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUpSQLite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	url := "sqlite://" + dbPath

	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := Up(context.Background(), log, url); err != nil {
		t.Fatalf("Up: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var version, engine string
	if err := db.QueryRow(`SELECT value FROM schema_meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != "1" {
		t.Fatalf("schema_version = %q, want %q", version, "1")
	}
	if err := db.QueryRow(`SELECT value FROM schema_meta WHERE key = 'engine'`).Scan(&engine); err != nil {
		t.Fatalf("query engine: %v", err)
	}
	if engine != "sqlite" {
		t.Fatalf("engine = %q, want %q", engine, "sqlite")
	}

	// Idempotency: second Up should be a no-op (no pending migrations).
	if err := Up(context.Background(), log, url); err != nil {
		t.Fatalf("second Up: %v", err)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
