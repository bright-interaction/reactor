package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestFindCachedOutputForInputFiltersByHash(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	// Seed a successful step row with input_hash h1.
	if _, err := j.RecordStepStart(ctx, "run_1", "fetch", 1, "k1", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordStepEnd(ctx, "run_1", "fetch", 1, json.RawMessage(`{"v":1}`), ""); err != nil {
		t.Fatal(err)
	}

	// Same input_hash matches.
	got, err := j.FindCachedOutputForInput(ctx, "run_1", "fetch", "k1", "h1")
	if err != nil {
		t.Fatalf("matching hash: %v", err)
	}
	if string(got) != `{"v":1}` {
		t.Fatalf("matching hash got %s, want {\"v\":1}", got)
	}

	// Different input_hash returns ErrNotFound so the supervisor
	// re-executes (live) or trips ErrReplayDivergence (replay).
	if _, err := j.FindCachedOutputForInput(ctx, "run_1", "fetch", "k1", "h2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drifted hash err = %v, want ErrNotFound", err)
	}

	// Empty input_hash falls back to ignoring the hash (legacy behaviour).
	if _, err := j.FindCachedOutputForInput(ctx, "run_1", "fetch", "k1", ""); err != nil {
		t.Fatalf("empty hash should match: %v", err)
	}
}

func TestHasCachedOutputAnyInput(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	any, err := j.HasCachedOutputAnyInput(ctx, "run_1", "fetch")
	if err != nil || any {
		t.Fatalf("before insert: any=%v err=%v, want false nil", any, err)
	}

	if _, err := j.RecordStepStart(ctx, "run_1", "fetch", 1, "k1", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordStepEnd(ctx, "run_1", "fetch", 1, json.RawMessage(`{"v":1}`), ""); err != nil {
		t.Fatal(err)
	}

	any, err = j.HasCachedOutputAnyInput(ctx, "run_1", "fetch")
	if err != nil || !any {
		t.Fatalf("after insert: any=%v err=%v, want true nil", any, err)
	}
}

func TestDeleteWorkflowRefusesActiveRuns(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	// run_1 is in 'running' status from CreateRun in the fixture.
	if err := j.DeleteWorkflow(ctx, "wf_1"); !errors.Is(err, ErrWorkflowBusy) {
		t.Fatalf("delete with active run err = %v, want ErrWorkflowBusy", err)
	}

	// Move run to a terminal state.
	if err := j.MarkRunFinished(ctx, "run_1", "succeeded"); err != nil {
		t.Fatal(err)
	}
	if err := j.DeleteWorkflow(ctx, "wf_1"); err != nil {
		t.Fatalf("delete after terminal: %v", err)
	}
}
