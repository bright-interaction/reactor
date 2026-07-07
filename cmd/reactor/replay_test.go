package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCmdReplayShowsTimeline(t *testing.T) {
	t.Parallel()
	url, j := newSeededDB(t)

	// Seed two recorded steps + a finalised run.
	ctx := context.Background()
	if _, err := j.RecordStepStart(ctx, "run_t", "fetch", 1, "k1", "h"); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordStepEnd(ctx, "run_t", "fetch", 1, json.RawMessage(`"ok"`), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordStepStart(ctx, "run_t", "send", 1, "k2", "h"); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordStepEnd(ctx, "run_t", "send", 1, json.RawMessage(`"sent"`), ""); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkRunFinished(ctx, "run_t", "succeeded"); err != nil {
		t.Fatal(err)
	}

	if err := cmdReplay(ctx, discardLogger(), []string{"--db", url, "run_t"}); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func TestCmdReplayMissingRun(t *testing.T) {
	t.Parallel()
	url, _ := newSeededDB(t)
	err := cmdReplay(context.Background(), discardLogger(), []string{"--db", url, "run_does_not_exist"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v, want 'not found' error", err)
	}
}

func TestCmdReplayRefusesRunningRun(t *testing.T) {
	t.Parallel()
	url, _ := newSeededDB(t)
	// run_t was created in newSeededDB and is still status='running'.
	err := cmdReplay(context.Background(), discardLogger(), []string{"--db", url, "run_t"})
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("got %v, want 'still running' error", err)
	}
}
