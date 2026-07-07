package journal

import (
	"context"
	"fmt"
	"time"
)

// This file is the usage-metering ledger that backs per-tenant billing. One
// run_usage row is written when a run reaches a terminal status, recording the
// tenant, workflow, wall-clock seconds, and step count. Writes are
// best-effort: a metering failure logs a warning but never fails the run.

// RecordRunUsage writes (or refreshes) the metering row for a terminal run. It
// reads the denormalized tenant + timing off the run and counts its steps.
// Idempotent via ON CONFLICT so a replay or re-finalize does not double count.
func (j *Journal) RecordRunUsage(ctx context.Context, runID, status string) error {
	var tenantID, workflowID string
	var startedAny, finishedAny any
	row := j.db.QueryRowContext(ctx,
		j.bind(`SELECT tenant_id, workflow_id, started_at, finished_at FROM runs WHERE id = $1`), runID)
	if err := row.Scan(&tenantID, &workflowID, &startedAny, &finishedAny); err != nil {
		return fmt.Errorf("journal: usage read run: %w", err)
	}
	started := j.anyTime(startedAny)
	finished := j.anyTime(finishedAny)
	var secs float64
	if !started.IsZero() && !finished.IsZero() && finished.After(started) {
		secs = finished.Sub(started).Seconds()
	}
	steps, activeSecs, err := j.stepStats(ctx, runID)
	if err != nil {
		return err
	}
	const q = `INSERT INTO run_usage
		(run_id, tenant_id, workflow_id, status, run_seconds, active_seconds, step_count, started_at, finished_at, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (run_id) DO UPDATE SET
			status = excluded.status, run_seconds = excluded.run_seconds,
			active_seconds = excluded.active_seconds, step_count = excluded.step_count,
			finished_at = excluded.finished_at, recorded_at = excluded.recorded_at`
	if _, err := j.db.ExecContext(ctx, j.bind(q),
		runID, tenantID, workflowID, status, secs, activeSecs, steps,
		j.timeArg(started), j.timeArg(finished), j.now()); err != nil {
		return fmt.Errorf("journal: record usage: %w", err)
	}
	return nil
}

// stepStats returns a run's step count and ACTIVE compute seconds: the sum of
// each step's own duration (finished - started). This is the billable compute
// meter -- it excludes time the run spent suspended (a long Sleep exits the
// subprocess and opens no step), so a workflow that waits days is not billed
// for the wait. Summed in Go to stay engine-portable (no EXTRACT/julianday).
func (j *Journal) stepStats(ctx context.Context, runID string) (count int, activeSeconds float64, err error) {
	rows, err := j.db.QueryContext(ctx,
		j.bind(`SELECT started_at, finished_at FROM steps WHERE run_id = $1`), runID)
	if err != nil {
		return 0, 0, fmt.Errorf("journal: step stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var startedAny, finishedAny any
		if err := rows.Scan(&startedAny, &finishedAny); err != nil {
			return 0, 0, err
		}
		count++
		st := j.anyTime(startedAny)
		fin := j.anyTime(finishedAny)
		if !st.IsZero() && !fin.IsZero() && fin.After(st) {
			activeSeconds += fin.Sub(st).Seconds()
		}
	}
	return count, activeSeconds, rows.Err()
}

// recordUsageBestEffort records usage and logs (never returns) on failure, so
// metering can never fail the terminal-status update that called it.
func (j *Journal) recordUsageBestEffort(ctx context.Context, runID, status string) {
	if err := j.RecordRunUsage(ctx, runID, status); err != nil {
		j.log.Warn("journal: run usage not recorded", "run_id", runID, "status", status, "err", err)
	}
}

// timeArg returns the engine-appropriate timestamp value, or nil for a zero
// time (so the column is stored NULL).
func (j *Journal) timeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return j.formatTime(t)
}

// TenantUsage is a tenant's metered totals over a period. RunSeconds is
// wall-clock (diagnostic); ActiveSeconds is billable compute (excludes time
// spent suspended).
type TenantUsage struct {
	TenantID      string  `json:"tenant_id"`
	Runs          int     `json:"runs"`
	RunSeconds    float64 `json:"run_seconds"`
	ActiveSeconds float64 `json:"active_seconds"`
	Steps         int     `json:"steps"`
}

// TenantUsageSince aggregates a tenant's metered usage recorded at or after t
// (pass the start of the billing period).
func (j *Journal) TenantUsageSince(ctx context.Context, tenantID string, t time.Time) (TenantUsage, error) {
	u := TenantUsage{TenantID: tenantID}
	err := j.db.QueryRowContext(ctx, j.bind(
		`SELECT COUNT(*), COALESCE(SUM(run_seconds), 0), COALESCE(SUM(active_seconds), 0), COALESCE(SUM(step_count), 0)
		 FROM run_usage WHERE tenant_id = $1 AND recorded_at >= $2`),
		tenantID, j.formatTime(t)).Scan(&u.Runs, &u.RunSeconds, &u.ActiveSeconds, &u.Steps)
	if err != nil {
		return TenantUsage{}, fmt.Errorf("journal: tenant usage: %w", err)
	}
	return u, nil
}

// StartOfMonthUTC is the first instant of the current calendar month (UTC),
// the default metering + monthly-quota window. Exported for callers (UI) that
// need the same boundary.
func StartOfMonthUTC() time.Time { return startOfMonthUTC() }
