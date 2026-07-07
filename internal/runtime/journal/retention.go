package journal

import (
	"context"
	"fmt"
	"time"
)

// This file is the GDPR/data-retention surface: an opt-in purge of old run
// history, plus per-tenant data export (portability) and erasure.

// terminalRunPredicate matches finished runs (not running/suspended/queued)
// older than the bound parameter. COALESCE(finished_at, created_at) so a run
// that finished without a finished_at still ages out by creation time.
const terminalRunPredicate = `status NOT IN ('running','suspended','queued') AND COALESCE(finished_at, created_at) < $1`

// PurgeTerminalRunsOlderThan deletes finished runs (and their steps, leases,
// schedules, logs via cascade; dead_letter + parent refs handled explicitly)
// older than cutoff. run_usage is intentionally kept (billing aggregates, no
// run payloads). Returns the number of runs removed. Off unless an operator
// sets a retention period.
func (j *Journal) PurgeTerminalRunsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	cut := j.formatTime(cutoff.UTC())
	target := `(SELECT id FROM runs WHERE ` + terminalRunPredicate + `)`

	// Detach survivors whose parent is being deleted (parent_run_id has no
	// cascade), then remove dead_letter (no cascade) before the runs delete
	// cascades the rest.
	if _, err := tx.ExecContext(ctx, j.bind(`UPDATE runs SET parent_run_id = NULL WHERE parent_run_id IN `+target), cut); err != nil {
		return 0, fmt.Errorf("journal: purge detach children: %w", err)
	}
	if _, err := tx.ExecContext(ctx, j.bind(`DELETE FROM dead_letter WHERE run_id IN `+target), cut); err != nil {
		return 0, fmt.Errorf("journal: purge dead_letter: %w", err)
	}
	res, err := tx.ExecContext(ctx, j.bind(`DELETE FROM runs WHERE `+terminalRunPredicate), cut)
	if err != nil {
		return 0, fmt.Errorf("journal: purge runs: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// TenantExport is a tenant's portable data snapshot (GDPR data portability).
type TenantExport struct {
	TenantID  string      `json:"tenant_id"`
	Tenant    Tenant      `json:"tenant"`
	Workflows []Workflow  `json:"workflows"`
	Runs      []RunInfo   `json:"runs"`
	Usage     TenantUsage `json:"usage_to_date"`
}

// ExportTenantData gathers a tenant's data for export. Runs are capped at
// maxRuns (newest first) to keep the payload bounded.
func (j *Journal) ExportTenantData(ctx context.Context, tenantID string, maxRuns int) (TenantExport, error) {
	if maxRuns <= 0 {
		maxRuns = 1000
	}
	exp := TenantExport{TenantID: tenantID}
	if t, err := j.GetTenant(ctx, tenantID); err == nil {
		exp.Tenant = t
	}
	wfs, err := j.ListWorkflowsByTenant(ctx, tenantID)
	if err != nil {
		return TenantExport{}, err
	}
	exp.Workflows = wfs
	runs, err := j.ListRuns(ctx, RunFilter{TenantID: tenantID, Limit: maxRuns})
	if err != nil {
		return TenantExport{}, err
	}
	exp.Runs = runs
	exp.Usage, _ = j.TenantUsageSince(ctx, tenantID, time.Time{})
	return exp, nil
}

// TenantErasure reports what an erase removed.
type TenantErasure struct {
	Runs  int64 `json:"runs"`
	Usage int64 `json:"usage_rows"`
}

// EraseTenantData deletes a tenant's run history + usage ledger (GDPR right to
// erasure). Workflows, credentials, and connections are left in place (they are
// the tenant's configuration, not personal run data); delete the tenant +
// those separately to fully offboard. Returns the counts removed.
func (j *Journal) EraseTenantData(ctx context.Context, tenantID string) (TenantErasure, error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return TenantErasure{}, err
	}
	defer tx.Rollback()
	target := `(SELECT id FROM runs WHERE tenant_id = $1)`
	if _, err := tx.ExecContext(ctx, j.bind(`UPDATE runs SET parent_run_id = NULL WHERE parent_run_id IN `+target), tenantID); err != nil {
		return TenantErasure{}, fmt.Errorf("journal: erase detach children: %w", err)
	}
	if _, err := tx.ExecContext(ctx, j.bind(`DELETE FROM dead_letter WHERE run_id IN `+target), tenantID); err != nil {
		return TenantErasure{}, fmt.Errorf("journal: erase dead_letter: %w", err)
	}
	runRes, err := tx.ExecContext(ctx, j.bind(`DELETE FROM runs WHERE tenant_id = $1`), tenantID)
	if err != nil {
		return TenantErasure{}, fmt.Errorf("journal: erase runs: %w", err)
	}
	usageRes, err := tx.ExecContext(ctx, j.bind(`DELETE FROM run_usage WHERE tenant_id = $1`), tenantID)
	if err != nil {
		return TenantErasure{}, fmt.Errorf("journal: erase usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TenantErasure{}, err
	}
	var e TenantErasure
	e.Runs, _ = runRes.RowsAffected()
	e.Usage, _ = usageRes.RowsAffected()
	return e, nil
}
