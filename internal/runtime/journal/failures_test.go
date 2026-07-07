package journal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestListFailedRuns(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	mkWorkflowTenant(t, j, ctx, "wf_f", "ten")
	if err := j.CreateRun(ctx, "run_f", "wf_f", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// A failed step with an error message.
	if _, err := j.db.ExecContext(ctx, j.bind(
		`INSERT INTO steps (run_id, step_name, attempt, input_hash, status, error_text, started_at, finished_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`),
		"run_f", "boom", 1, "h", "failed", "kaboom: upstream 500", j.now(), j.now()); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkRunFinished(ctx, "run_f", "failed"); err != nil {
		t.Fatal(err)
	}

	failed, err := j.ListFailedRuns(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed runs = %d, want 1", len(failed))
	}
	f := failed[0]
	if f.RunID != "run_f" || f.StepName != "boom" || !strings.Contains(f.Error, "kaboom") {
		t.Fatalf("failed run = %+v", f)
	}

	// Tenant scoping.
	if got, _ := j.ListFailedRuns(ctx, "ten", 10); len(got) != 1 {
		t.Fatalf("tenant 'ten' failed = %d, want 1", len(got))
	}
	if got, _ := j.ListFailedRuns(ctx, "other", 10); len(got) != 0 {
		t.Fatalf("tenant 'other' failed = %d, want 0 (isolation)", len(got))
	}
	if n, _ := j.CountFailedRuns(ctx, "ten"); n != 1 {
		t.Fatalf("count failed (ten) = %d, want 1", n)
	}
}

func TestTopFailingWorkflows(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_a", "a", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateWorkflow(ctx, "wf_b", "b", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mk := func(id, wf string) {
		if err := j.CreateRun(ctx, id, wf, "manual", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := j.MarkRunFinished(ctx, id, "failed"); err != nil {
			t.Fatal(err)
		}
	}
	mk("a1", "wf_a")
	mk("a2", "wf_a")
	mk("b1", "wf_b")

	top, err := j.TopFailingWorkflows(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].WorkflowID != "wf_a" || top[0].Count != 2 {
		t.Fatalf("top failing = %+v, want wf_a leading with 2", top)
	}
}
