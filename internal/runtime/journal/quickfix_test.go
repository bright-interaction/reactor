package journal

import (
	"context"
	"encoding/json"
	"testing"
)

// TestPercentileMsNearestRank pins the off-by-one fix: p95 of 1..100 is
// 95 (nearest-rank), not 96.
func TestPercentileMsNearestRank(t *testing.T) {
	vals := make([]int64, 100)
	for i := range vals {
		vals[i] = int64(i + 1)
	}
	if got := percentileMs(vals, 0.95); got != 95 {
		t.Fatalf("p95(1..100) = %d, want 95", got)
	}
	if got := percentileMs(vals, 0.50); got != 50 {
		t.Fatalf("p50(1..100) = %d, want 50", got)
	}
	if got := percentileMs(vals, 1.0); got != 100 {
		t.Fatalf("p100(1..100) = %d, want 100", got)
	}
	if got := percentileMs(nil, 0.95); got != 0 {
		t.Fatalf("p95(empty) = %d, want 0", got)
	}
}

// TestDeleteWorkflowCleansOrphanChainTriggers proves deleting a source
// workflow removes chain triggers that pointed at it (which live in
// another workflow's config_json, not a FK column).
func TestDeleteWorkflowCleansOrphanChainTriggers(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	for _, slug := range []string{"wf_src", "wf_down"} {
		if err := j.CreateWorkflow(ctx, slug, slug, "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	// wf_down runs after wf_src completes.
	if _, err := j.CreateChainTrigger(ctx, "wf_down", "wf_src", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := j.ChainTriggersForSource(ctx, "wf_src", "succeeded"); len(got) != 1 {
		t.Fatalf("expected 1 chain trigger before delete, got %d", len(got))
	}
	// Delete the source workflow; its dangling chain trigger must go too.
	if err := j.DeleteWorkflow(ctx, "wf_src"); err != nil {
		t.Fatal(err)
	}
	if got, _ := j.ChainTriggersForSource(ctx, "wf_src", "succeeded"); len(got) != 0 {
		t.Fatalf("orphan chain trigger not cleaned: %d remain", len(got))
	}
}
