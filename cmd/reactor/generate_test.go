package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBriefPositional(t *testing.T) {
	t.Parallel()
	got, err := resolveBrief("", "", []string{"send", "welcome", "email"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "send welcome email" {
		t.Fatalf("got %q, want joined positional", got)
	}
}

func TestResolveBriefFlagBeatsPositional(t *testing.T) {
	t.Parallel()
	got, err := resolveBrief("flag wins", "", []string{"positional"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "flag wins" {
		t.Fatalf("got %q, want %q", got, "flag wins")
	}
}

func TestResolveBriefFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(path, []byte("file brief content"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBrief("flag is overridden", path, []string{"positional"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "file brief content") {
		t.Fatalf("got %q, want file content", got)
	}
}

func TestResolveBriefMissingErrors(t *testing.T) {
	t.Parallel()
	if _, err := resolveBrief("", "", nil); err == nil {
		t.Fatal("expected error when brief missing")
	}
}

func TestResolveBriefMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := resolveBrief("", "/no/such/file.md", nil); err == nil {
		t.Fatal("expected error when brief-file missing")
	}
}

func TestNoopCommitter(t *testing.T) {
	t.Parallel()
	if err := (noopCommitter{}).Commit(context.TODO(), "", "", ""); err != nil {
		t.Fatalf("noopCommitter.Commit returned %v, want nil", err)
	}
}
