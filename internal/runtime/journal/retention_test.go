package journal

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func setRunTimes(t *testing.T, j *Journal, ctx context.Context, runID string, ts time.Time) {
	t.Helper()
	v := j.formatTime(ts)
	if _, err := j.db.ExecContext(ctx, j.bind(`UPDATE runs SET created_at = $1, finished_at = $2 WHERE id = $3`), v, v, runID); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeTerminalRunsOlderThan(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	mkWorkflowTenant(t, j, ctx, "wf_r", "r")

	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	// Old finished run, with a dead_letter row (no FK cascade -> must be
	// deleted explicitly, otherwise the purge would FK-violate).
	if err := j.CreateRun(ctx, "old", "wf_r", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	_ = j.MarkRunFinished(ctx, "old", "failed_dlq")
	setRunTimes(t, j, ctx, "old", old)
	if _, err := j.db.ExecContext(ctx, j.bind(
		`INSERT INTO dead_letter (id, run_id, step_name, error_text, payload, moved_at) VALUES ($1,$2,$3,$4,$5,$6)`),
		"dl1", "old", "step", "boom", outputArg(json.RawMessage(`{}`), j.engine), j.now()); err != nil {
		t.Fatal(err)
	}
	// Recent finished run + a still-running run: both must survive.
	if err := j.CreateRun(ctx, "recent", "wf_r", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	_ = j.MarkRunFinished(ctx, "recent", "succeeded")
	if err := j.CreateRun(ctx, "live", "wf_r", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	setRunTimes(t, j, ctx, "live", old) // old, but still running -> not purged

	n, err := j.PurgeTerminalRunsOlderThan(ctx, time.Now().UTC().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1 (only the old terminal run)", n)
	}
	survivors := map[string]bool{}
	for _, id := range []string{"old", "recent", "live"} {
		var x string
		err := j.db.QueryRowContext(ctx, j.bind(`SELECT id FROM runs WHERE id = $1`), id).Scan(&x)
		survivors[id] = err == nil
	}
	if survivors["old"] || !survivors["recent"] || !survivors["live"] {
		t.Fatalf("purge kept the wrong rows: %v", survivors)
	}
}

func TestExportAndEraseTenant(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	mkWorkflowTenant(t, j, ctx, "wf_e", "erase-me")
	if err := j.CreateRun(ctx, "e1", "wf_e", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	_ = j.MarkRunFinished(ctx, "e1", "succeeded")

	// Export gathers the tenant's workflows + runs.
	exp, err := j.ExportTenantData(ctx, "erase-me", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Workflows) != 1 || len(exp.Runs) != 1 {
		t.Fatalf("export = %d workflows / %d runs, want 1/1", len(exp.Workflows), len(exp.Runs))
	}

	// Erase removes the run history (+ usage), leaves other tenants alone.
	if err := j.CreateRun(ctx, "keep", "wf_1", "manual", json.RawMessage(`{}`)); err != nil { // wf_1 = seeded default tenant
		t.Fatal(err)
	}
	er, err := j.EraseTenantData(ctx, "erase-me")
	if err != nil {
		t.Fatal(err)
	}
	if er.Runs != 1 {
		t.Fatalf("erased %d runs, want 1", er.Runs)
	}
	if n, _ := j.CountRunningForTenant(ctx, "erase-me"); n != 0 {
		t.Fatalf("tenant still has runs after erase")
	}
	var keep string
	if err := j.db.QueryRowContext(ctx, j.bind(`SELECT id FROM runs WHERE id = 'keep'`)).Scan(&keep); err != nil {
		t.Fatal("erase wrongly removed another tenant's run")
	}
}
