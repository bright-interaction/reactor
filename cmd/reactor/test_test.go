package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestCmdTestRequiresSlug(t *testing.T) {
	t.Parallel()
	err := cmdTest(context.Background(), slog.Default(), nil)
	if err == nil || !strings.Contains(err.Error(), "missing <slug>") {
		t.Fatalf("got %v, want missing-slug error", err)
	}
}

func TestCmdTestRequiresMode(t *testing.T) {
	t.Parallel()
	err := cmdTest(context.Background(), slog.Default(), []string{"some-slug", "--db=sqlite:///tmp/x.db", "--root=/tmp/x"})
	if err == nil || !strings.Contains(err.Error(), "--against-run") {
		t.Fatalf("got %v, want --against-run-or-latest error", err)
	}
}

func TestCmdTestRejectsBothModes(t *testing.T) {
	t.Parallel()
	err := cmdTest(context.Background(), slog.Default(), []string{"some-slug", "--db=sqlite:///tmp/x.db", "--root=/tmp/x", "--against-run=run_a", "--latest"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("got %v, want mutually-exclusive error", err)
	}
}
