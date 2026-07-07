package journal

import (
	"context"
	"testing"
	"time"
)

func TestWorkerRegistry(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	if err := j.UpsertWorkerHeartbeat(ctx, "w1", 8); err != nil {
		t.Fatal(err)
	}
	if err := j.UpsertWorkerHeartbeat(ctx, "w2", 16); err != nil {
		t.Fatal(err)
	}

	workers, err := j.ListActiveWorkers(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 2 {
		t.Fatalf("ListActiveWorkers = %d, want 2", len(workers))
	}
	if count, capacity, _ := j.WorkerCapacity(ctx, time.Minute); count != 2 || capacity != 24 {
		t.Fatalf("capacity = %d workers / %d concurrency, want 2 / 24", count, capacity)
	}

	// Re-heartbeat with a new concurrency updates in place (no dupe row).
	if err := j.UpsertWorkerHeartbeat(ctx, "w1", 4); err != nil {
		t.Fatal(err)
	}
	if count, capacity, _ := j.WorkerCapacity(ctx, time.Minute); count != 2 || capacity != 20 {
		t.Fatalf("after re-heartbeat: %d / %d, want 2 / 20", count, capacity)
	}

	// Graceful leave.
	if err := j.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if count, _, _ := j.WorkerCapacity(ctx, time.Minute); count != 1 {
		t.Fatalf("after delete: %d workers, want 1", count)
	}

	// Prune: a future-shifted cutoff treats the remaining row as stale.
	n, err := j.PruneStaleWorkers(ctx, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	if count, _, _ := j.WorkerCapacity(ctx, time.Minute); count != 0 {
		t.Fatalf("after prune: %d workers, want 0", count)
	}
}
