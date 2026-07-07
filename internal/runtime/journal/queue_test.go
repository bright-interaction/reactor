package journal

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestQueueClaimLeaseReap(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_q", "queue-demo", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// Enqueue two runs.
	if err := j.CreateQueuedRun(ctx, "run_a", "wf_q", "webhook", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateQueuedRun(ctx, "run_b", "wf_q", "webhook", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if n, _ := j.CountQueued(ctx); n != 2 {
		t.Fatalf("CountQueued = %d, want 2", n)
	}

	// Claim one: FIFO (run_a first), flips to running, writes a lease.
	ids, err := j.ClaimQueuedRuns(ctx, "worker-1", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "run_a" {
		t.Fatalf("claim = %v, want [run_a]", ids)
	}
	if info, _ := j.GetRun(ctx, "run_a"); info.Status != "running" {
		t.Fatalf("claimed run status = %q, want running", info.Status)
	}
	if n, _ := j.CountQueued(ctx); n != 1 {
		t.Fatalf("CountQueued after one claim = %d, want 1", n)
	}

	// Extend + release the lease.
	if err := j.ExtendLease(ctx, "run_a", "worker-1", 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := j.ReleaseLease(ctx, "run_a"); err != nil {
		t.Fatal(err)
	}

	// Claim run_b with an already-expired lease, then reap it: the run
	// should requeue (worker died mid-run) and re-enter the queue.
	ids, err = j.ClaimQueuedRuns(ctx, "worker-2", 5, -time.Hour)
	if err != nil || len(ids) != 1 || ids[0] != "run_b" {
		t.Fatalf("claim run_b = %v, %v", ids, err)
	}
	reaped, err := j.ReapExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}
	if info, _ := j.GetRun(ctx, "run_b"); info.Status != "queued" {
		t.Fatalf("reaped run status = %q, want queued (requeued)", info.Status)
	}
	if n, _ := j.CountQueued(ctx); n != 1 {
		t.Fatalf("CountQueued after reap = %d, want 1", n)
	}

	// An empty queue claim returns nothing, no error.
	_ = j.ReleaseLease(ctx, "run_b")
	_, _ = j.ClaimQueuedRuns(ctx, "w", 10, time.Minute) // drains run_b
	got, err := j.ClaimQueuedRuns(ctx, "w", 10, time.Minute)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty claim = %v, %v; want nil/nil", got, err)
	}
}
