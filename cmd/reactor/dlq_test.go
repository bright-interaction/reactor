package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/reactor/internal/migrate"
	"github.com/bright-interaction/reactor/internal/runtime/journal"

	"io"
	"log/slog"
)

// captureStdout reroutes os.Stdout for the duration of fn and returns the
// captured text. Used to assert CLI output without spawning subprocesses.
//
// We don't actually swap os.Stdout here; the dlq + replay commands write
// straight to fmt.Println / tabwriter at os.Stdout. To avoid coupling to
// stdout swapping, the tests below use the journal directly.

func newSeededDB(t *testing.T) (string, *journal.Journal) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "j.db")
	url := "sqlite://" + dbPath

	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := migrate.Up(context.Background(), silent, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, _, err := migrate.Open(url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	j := journal.New(db, journal.EngineSQLite)
	if err := j.CreateWorkflow(context.Background(), "wf_t", "demo", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if err := j.CreateRun(context.Background(), "run_t", "wf_t", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("run: %v", err)
	}
	return url, j
}

func TestCmdDLQListEmpty(t *testing.T) {
	t.Parallel()
	url, _ := newSeededDB(t)
	if err := cmdDLQ(context.Background(), discardLogger(), []string{"list", "--db", url}); err != nil {
		t.Fatalf("list empty: %v", err)
	}
}

func TestCmdDLQListAndShow(t *testing.T) {
	t.Parallel()
	url, j := newSeededDB(t)

	if err := j.MoveStepToDeadLetter(context.Background(), "run_t", "send", "boom 502", json.RawMessage(`{"to":"x"}`)); err != nil {
		t.Fatal(err)
	}
	items, err := j.ListDeadLetterItems(context.Background(), 10, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("seed: items=%v err=%v", items, err)
	}
	id := items[0].ID

	if err := cmdDLQ(context.Background(), discardLogger(), []string{"list", "--db", url, "--limit", "5"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := cmdDLQ(context.Background(), discardLogger(), []string{"show", "--db", url, id}); err != nil {
		t.Fatalf("show: %v", err)
	}
	if err := cmdDLQ(context.Background(), discardLogger(), []string{"show", "--db", url, "dlq_missing"}); err == nil {
		t.Fatal("show missing should error")
	}
}

func TestCmdDLQUnknownSub(t *testing.T) {
	t.Parallel()
	if err := cmdDLQ(context.Background(), discardLogger(), []string{"frobnicate"}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("got %v, want unknown subcommand", err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
