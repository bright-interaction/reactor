package journal

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func addSteps(t *testing.T, j *Journal, ctx context.Context, runID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := j.db.ExecContext(ctx, j.bind(
			`INSERT INTO steps (run_id, step_name, attempt, input_hash, status, started_at) VALUES ($1,$2,$3,$4,$5,$6)`),
			runID, "s"+itoa(i), 1, "h", "succeeded", j.now()); err != nil {
			t.Fatal(err)
		}
	}
}

// addTimedStep inserts a step with explicit start/finish so active_seconds is
// deterministic.
func addTimedStep(t *testing.T, j *Journal, ctx context.Context, runID, name string, start, finish time.Time) {
	t.Helper()
	if _, err := j.db.ExecContext(ctx, j.bind(
		`INSERT INTO steps (run_id, step_name, attempt, input_hash, status, started_at, finished_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`),
		runID, name, 1, "h", "succeeded", j.formatTime(start), j.formatTime(finish)); err != nil {
		t.Fatal(err)
	}
}

// TestMeteringActiveSecondsExcludesWait is the billing-correctness guard: a run
// whose wall-clock spans a long wait must bill only its active step time, not
// the idle suspend.
func TestMeteringActiveSecondsExcludesWait(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	mkWorkflowTenant(t, j, ctx, "wf_w", "w")
	if err := j.CreateRun(ctx, "run_w", "wf_w", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Two steps of 2s each = 4s active. The run's wall-clock spans a 7-day
	// wait between them (the suspend), which must NOT be billed.
	base := time.Now().UTC().Add(-7 * 24 * time.Hour)
	addTimedStep(t, j, ctx, "run_w", "before_sleep", base, base.Add(2*time.Second))
	addTimedStep(t, j, ctx, "run_w", "after_sleep",
		time.Now().UTC().Add(-2*time.Second), time.Now().UTC())
	if err := j.MarkRunFinished(ctx, "run_w", "succeeded"); err != nil {
		t.Fatal(err)
	}

	u, err := j.TenantUsageSince(ctx, "w", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Billable compute = 4s of active steps, NOT the 7-day gap between them.
	if u.ActiveSeconds < 3.5 || u.ActiveSeconds > 4.5 {
		t.Fatalf("active_seconds = %.2f, want ~4 (the 7-day wait excluded)", u.ActiveSeconds)
	}
}

func TestMeteringOnFinish(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	mkWorkflowTenant(t, j, ctx, "wf_m", "m")
	if err := j.CreateRun(ctx, "run_m", "wf_m", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	addSteps(t, j, ctx, "run_m", 3)

	if err := j.MarkRunFinished(ctx, "run_m", "succeeded"); err != nil {
		t.Fatal(err)
	}

	u, err := j.TenantUsageSince(ctx, "m", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if u.Runs != 1 || u.Steps != 3 {
		t.Fatalf("usage = %+v, want 1 run / 3 steps", u)
	}
	if u.RunSeconds < 0 {
		t.Fatalf("run_seconds negative: %v", u.RunSeconds)
	}

	// A tenant with no metered runs aggregates to zero.
	z, err := j.TenantUsageSince(ctx, "other", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if z.Runs != 0 || z.Steps != 0 || z.RunSeconds != 0 {
		t.Fatalf("empty usage = %+v, want zeros", z)
	}
}

func TestMeteringOnCancel(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	mkWorkflowTenant(t, j, ctx, "wf_x", "x")
	if err := j.CreateRun(ctx, "run_x", "wf_x", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.FinalizeCancel(ctx, "run_x"); err != nil {
		t.Fatal(err)
	}
	u, err := j.TenantUsageSince(ctx, "x", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if u.Runs != 1 {
		t.Fatalf("cancelled run not metered: %+v", u)
	}

	// Re-finalize is idempotent (ON CONFLICT), so usage stays 1 run.
	_ = j.RecordRunUsage(ctx, "run_x", "cancelled")
	if u, _ := j.TenantUsageSince(ctx, "x", time.Now().Add(-time.Hour)); u.Runs != 1 {
		t.Fatalf("metering not idempotent: %+v", u)
	}
}
