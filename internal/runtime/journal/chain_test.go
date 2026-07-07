package journal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCreateChainTriggerRejectsSelfLoop(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	if _, err := j.CreateChainTrigger(context.Background(), "wf_1", "wf_1", "succeeded"); err == nil {
		t.Fatal("self-loop should have been rejected")
	}
}

// TestCreateChainTriggerRejectsCycle is the regression test for the
// ship-blocker where only direct self-loops were rejected: an A->B->C->A
// chain would dispatch forever. The third edge closing the loop must be
// refused at create time.
func TestCreateChainTriggerRejectsCycle(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	for _, slug := range []string{"wf_a", "wf_b", "wf_c"} {
		if err := j.CreateWorkflow(ctx, slug, slug, "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	// a -> b (b fires when a completes), b -> c. Both fine.
	if _, err := j.CreateChainTrigger(ctx, "wf_b", "wf_a", ""); err != nil {
		t.Fatalf("a->b: %v", err)
	}
	if _, err := j.CreateChainTrigger(ctx, "wf_c", "wf_b", ""); err != nil {
		t.Fatalf("b->c: %v", err)
	}
	// c -> a closes the cycle a->b->c->a and must be rejected.
	if _, err := j.CreateChainTrigger(ctx, "wf_a", "wf_c", ""); err == nil {
		t.Fatal("cycle-closing chain trigger should have been rejected")
	}
}

func TestCreateChainTriggerRoundTrip(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_down", "downstream", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	id, err := j.CreateChainTrigger(ctx, "wf_down", "wf_1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "trg_") {
		t.Fatalf("id = %q", id)
	}

	got, err := j.ChainTriggersForSource(ctx, "wf_1", "succeeded")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WorkflowID != "wf_down" {
		t.Fatalf("got = %+v", got)
	}
	// Default on_statuses = succeeded only; failed should not fire.
	got, _ = j.ChainTriggersForSource(ctx, "wf_1", "failed")
	if len(got) != 0 {
		t.Fatalf("failed should not fire under default succeeded-only route: %+v", got)
	}
}

func TestChainTriggerOnStatusesCSV(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_down", "downstream", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.CreateChainTrigger(ctx, "wf_down", "wf_1", "succeeded,failed_dlq"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"succeeded", "failed_dlq"} {
		got, _ := j.ChainTriggersForSource(ctx, "wf_1", status)
		if len(got) != 1 {
			t.Fatalf("%s should fire: %+v", status, got)
		}
	}
	got, _ := j.ChainTriggersForSource(ctx, "wf_1", "failed")
	if len(got) != 0 {
		t.Fatalf("failed should not fire: %+v", got)
	}
}

func TestChainTriggersDownstreamOfAndUpstreamOf(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_down", "downstream", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.CreateChainTrigger(ctx, "wf_down", "wf_1", "succeeded"); err != nil {
		t.Fatal(err)
	}

	down, err := j.ChainTriggersDownstreamOf(ctx, "wf_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(down) != 1 || down[0].DownstreamSlug != "downstream" {
		t.Fatalf("downstream view = %+v", down)
	}

	up, err := j.ChainTriggersUpstreamOf(ctx, "wf_down")
	if err != nil {
		t.Fatal(err)
	}
	if len(up) != 1 || up[0].SourceWorkflowID != "wf_1" || up[0].SourceSlug != "demo" {
		t.Fatalf("upstream view = %+v", up)
	}
}
