package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestWorkflowEnabledRoundTrip proves SetWorkflowEnabled actually flips the
// flag IsWorkflowEnabled reads (the dispatch gate the audit found
// cosmetic), and that the engine-aware boolValue binding works.
func TestWorkflowEnabledRoundTrip(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_e", "enabled-demo", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// Default is enabled.
	if ok, err := j.IsWorkflowEnabled(ctx, "wf_e"); err != nil || !ok {
		t.Fatalf("fresh workflow should be enabled: ok=%v err=%v", ok, err)
	}
	// Disable -> reads back disabled.
	if err := j.SetWorkflowEnabled(ctx, "wf_e", false); err != nil {
		t.Fatal(err)
	}
	if ok, err := j.IsWorkflowEnabled(ctx, "wf_e"); err != nil || ok {
		t.Fatalf("disabled workflow should read disabled: ok=%v err=%v", ok, err)
	}
	// Re-enable.
	if err := j.SetWorkflowEnabled(ctx, "wf_e", true); err != nil {
		t.Fatal(err)
	}
	if ok, err := j.IsWorkflowEnabled(ctx, "wf_e"); err != nil || !ok {
		t.Fatalf("re-enabled workflow should read enabled: ok=%v err=%v", ok, err)
	}
	// Unknown workflow.
	if _, err := j.IsWorkflowEnabled(ctx, "wf_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown workflow should return ErrNotFound, got %v", err)
	}
}
