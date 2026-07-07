package journal

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// FailedRun is one failed execution for the error log: the run plus the error
// text of its most recent failed step. PostmortemID is filled by the caller
// (the server links to a generated post-mortem when one exists).
type FailedRun struct {
	RunID      string
	WorkflowID string
	TenantID   string
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	StepName   string
	Error      string
}

// ListFailedRuns returns the most recent failed executions (status LIKE
// 'failed%', covering 'failed' + 'failed_dlq') with the error text of each
// run's latest failed step. When tenantID is non-empty it is scoped to that
// tenant (dashboard isolation). This is the error log's data source.
func (j *Journal) ListFailedRuns(ctx context.Context, tenantID string, limit int) ([]FailedRun, error) {
	if limit <= 0 {
		limit = 100
	}
	// Correlated subqueries (not LATERAL) keep this portable across Postgres +
	// SQLite. The error log is a bounded list, so the per-row lookup is fine.
	q := `SELECT r.id, r.workflow_id, r.tenant_id, r.status, r.started_at, r.finished_at,
		COALESCE((SELECT step_name FROM steps WHERE run_id = r.id AND status = 'failed' ORDER BY started_at DESC LIMIT 1), ''),
		COALESCE((SELECT error_text FROM steps WHERE run_id = r.id AND status = 'failed' ORDER BY started_at DESC LIMIT 1), '')
		FROM runs r
		WHERE r.status LIKE 'failed%'`
	args := []any{}
	pos := 1
	if tenantID != "" {
		q += fmt.Sprintf(" AND r.tenant_id = $%d", pos)
		args = append(args, tenantID)
		pos++
	}
	q += fmt.Sprintf(" ORDER BY r.finished_at DESC LIMIT $%d", pos)
	args = append(args, limit)

	rows, err := j.db.QueryContext(ctx, j.bind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("journal: list failed runs: %w", err)
	}
	defer rows.Close()
	var out []FailedRun
	for rows.Next() {
		var (
			fr               FailedRun
			started, finished sql.NullString
		)
		if err := rows.Scan(&fr.RunID, &fr.WorkflowID, &fr.TenantID, &fr.Status,
			&started, &finished, &fr.StepName, &fr.Error); err != nil {
			return nil, fmt.Errorf("journal: scan failed run: %w", err)
		}
		if started.Valid {
			if t, err := j.parseTime(started.String); err == nil {
				fr.StartedAt = t
			}
		}
		if finished.Valid {
			if t, err := j.parseTime(finished.String); err == nil {
				fr.FinishedAt = t
			}
		}
		out = append(out, fr)
	}
	return out, rows.Err()
}

// WorkflowFailureCount is one row of the recurring-failure summary.
type WorkflowFailureCount struct {
	WorkflowID string
	Count      int
}

// TopFailingWorkflows groups failed runs by workflow to surface recurring
// failure patterns (the workflows worth fixing first). Optionally tenant-scoped.
func (j *Journal) TopFailingWorkflows(ctx context.Context, tenantID string, limit int) ([]WorkflowFailureCount, error) {
	if limit <= 0 {
		limit = 10
	}
	q := `SELECT workflow_id, COUNT(*) AS n FROM runs WHERE status LIKE 'failed%'`
	args := []any{}
	pos := 1
	if tenantID != "" {
		q += fmt.Sprintf(" AND tenant_id = $%d", pos)
		args = append(args, tenantID)
		pos++
	}
	q += fmt.Sprintf(" GROUP BY workflow_id ORDER BY n DESC, workflow_id LIMIT $%d", pos)
	args = append(args, limit)
	rows, err := j.db.QueryContext(ctx, j.bind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("journal: top failing workflows: %w", err)
	}
	defer rows.Close()
	var out []WorkflowFailureCount
	for rows.Next() {
		var c WorkflowFailureCount
		if err := rows.Scan(&c.WorkflowID, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountFailedRuns counts failed executions, optionally scoped to a tenant.
func (j *Journal) CountFailedRuns(ctx context.Context, tenantID string) (int, error) {
	q := `SELECT COUNT(*) FROM runs WHERE status LIKE 'failed%'`
	args := []any{}
	if tenantID != "" {
		q += " AND tenant_id = $1"
		args = append(args, tenantID)
	}
	var n int
	if err := j.db.QueryRowContext(ctx, j.bind(q), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("journal: count failed runs: %w", err)
	}
	return n, nil
}
