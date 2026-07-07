package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRendersCronEcho(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := cmdNew(context.Background(), slog.Default(), []string{"cron-echo", "my-test", "--dest=" + dir}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"main.go", "dag.json", "README.md"} {
		path := filepath.Join(dir, "my-test", f)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
		if !strings.Contains(string(body), "my-test") {
			t.Fatalf("%s did not substitute slug", f)
		}
	}
}

func TestNewRefusesUnknownTemplate(t *testing.T) {
	t.Parallel()
	err := cmdNew(context.Background(), slog.Default(), []string{"does-not-exist", "my-test", "--dest", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unknown template") {
		t.Fatalf("got %v, want unknown-template error", err)
	}
}

func TestNewRefusesBadSlug(t *testing.T) {
	t.Parallel()
	err := cmdNew(context.Background(), slog.Default(), []string{"cron-echo", "../escape", "--dest", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("got %v, want slug regex error", err)
	}
}

func TestNewRefusesOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "exists"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := cmdNew(context.Background(), slog.Default(), []string{"cron-echo", "exists", "--dest=" + dir})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("got %v, want already-exists error", err)
	}
}

func TestNewAllTemplatesRender(t *testing.T) {
	t.Parallel()
	for _, tpl := range availableTemplates {
		t.Run(tpl, func(t *testing.T) {
			dir := t.TempDir()
			if err := cmdNew(context.Background(), slog.Default(), []string{tpl, "smoke", "--dest=" + dir}); err != nil {
				t.Fatalf("template %s failed: %v", tpl, err)
			}
		})
	}
}
