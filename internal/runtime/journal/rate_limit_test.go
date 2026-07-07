package journal

import (
	"context"
	"encoding/json"
	"testing"
)

func TestWorkflowRateLimit(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_rl", "rl", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// No limit set -> always allowed.
	if ok, limit, _ := j.CheckWorkflowRateLimit(ctx, "wf_rl"); !ok || limit != 0 {
		t.Fatalf("default = allowed/unlimited, got ok=%v limit=%d", ok, limit)
	}

	// Limit 2/min.
	if err := j.SetWorkflowRateLimit(ctx, "wf_rl", 2); err != nil {
		t.Fatal(err)
	}
	if n, _ := j.WorkflowRateLimit(ctx, "wf_rl"); n != 2 {
		t.Fatalf("rate limit = %d, want 2", n)
	}
	// One run this minute -> still under the cap.
	if err := j.CreateRun(ctx, "rl1", "wf_rl", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := j.CheckWorkflowRateLimit(ctx, "wf_rl"); !ok {
		t.Fatal("1 run under limit 2 should be allowed")
	}
	// Second run reaches the cap -> next is refused.
	if err := j.CreateRun(ctx, "rl2", "wf_rl", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if ok, limit, _ := j.CheckWorkflowRateLimit(ctx, "wf_rl"); ok || limit != 2 {
		t.Fatalf("at cap 2 should be refused, got ok=%v limit=%d", ok, limit)
	}
}
