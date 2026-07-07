package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WorkflowVersion is one row of workflow_versions. The current pointer
// (highest version) mirrors the workflows row's code_hash + sdk_version +
// dag_json so the legacy `reactor_get_workflow` path keeps working.
// Historical versions live here for audit + future "freeze runs to v3"
// enforcement.
type WorkflowVersion struct {
	WorkflowID string    `json:"workflow_id"`
	Version    int       `json:"version"`
	SDKVersion string    `json:"sdk_version"`
	CodeHash   string    `json:"code_hash"`
	DAG        []byte    `json:"dag_json"`
	CreatedAt  time.Time `json:"created_at"`
}

// RecordWorkflowVersion appends a new version row for the given workflow
// id. Returns the new version number (starts at 1, increments by 1 per
// call). Used by CreateWorkflow on first insert and by the future
// re-register path on every subsequent register.
func (j *Journal) RecordWorkflowVersion(ctx context.Context, workflowID, sdkVersion, codeHash string, dag json.RawMessage) (int, error) {
	if workflowID == "" {
		return 0, errors.New("journal: record workflow version: workflow_id required")
	}
	next, err := j.nextWorkflowVersion(ctx, workflowID)
	if err != nil {
		return 0, err
	}
	dg := outputArg(dag, j.engine)
	const q = `INSERT INTO workflow_versions
		(workflow_id, version, sdk_version, code_hash, dag_json, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := j.db.ExecContext(ctx, j.bind(q),
		workflowID, next, sdkVersion, codeHash, dg, j.now()); err != nil {
		return 0, fmt.Errorf("journal: insert workflow version: %w", err)
	}
	return next, nil
}

// CurrentWorkflowVersion returns the highest version number recorded
// for the workflow id, or (0, nil) if no version rows exist (which
// happens on fresh installs that registered workflows before 0008
// migrated; CreateWorkflow auto-fills the first row going forward).
func (j *Journal) CurrentWorkflowVersion(ctx context.Context, workflowID string) (int, error) {
	const q = `SELECT COALESCE(MAX(version), 0) FROM workflow_versions WHERE workflow_id = $1`
	var v int
	if err := j.db.QueryRowContext(ctx, j.bind(q), workflowID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("journal: current workflow version: %w", err)
	}
	return v, nil
}

// ListWorkflowVersions returns every recorded version for a workflow,
// newest first. Used by the dashboard to render a version timeline on
// the workflow detail page.
func (j *Journal) ListWorkflowVersions(ctx context.Context, workflowID string) ([]WorkflowVersion, error) {
	const q = `SELECT workflow_id, version, sdk_version, code_hash, dag_json, created_at
		FROM workflow_versions WHERE workflow_id = $1 ORDER BY version DESC`
	rows, err := j.db.QueryContext(ctx, j.bind(q), workflowID)
	if err != nil {
		return nil, fmt.Errorf("journal: list workflow versions: %w", err)
	}
	defer rows.Close()
	var out []WorkflowVersion
	for rows.Next() {
		var (
			v       WorkflowVersion
			dag     []byte
			created sql.NullString
		)
		if err := rows.Scan(&v.WorkflowID, &v.Version, &v.SDKVersion, &v.CodeHash, &dag, &created); err != nil {
			return nil, fmt.Errorf("journal: scan workflow version: %w", err)
		}
		v.DAG = dag
		if created.Valid {
			if t, err := j.parseTime(created.String); err == nil {
				v.CreatedAt = t
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetRunWorkflowVersion records the workflow version this run was
// dispatched against. Called by the dispatcher right after CreateRun
// so the run timeline can show "ran against v3" even after v4 lands.
func (j *Journal) SetRunWorkflowVersion(ctx context.Context, runID string, version int) error {
	const q = `UPDATE runs SET workflow_version = $1 WHERE id = $2`
	_, err := j.db.ExecContext(ctx, j.bind(q), version, runID)
	if err != nil {
		return fmt.Errorf("journal: set run workflow version: %w", err)
	}
	return nil
}

// nextWorkflowVersion peeks at the highest existing version for the
// workflow id and returns highest+1. Not atomic against concurrent
// RecordWorkflowVersion callers; the dispatcher + CLI both serialize
// register calls so this is fine for v0.1. v0.2 can move to a
// per-workflow advisory lock if concurrent registers become real.
func (j *Journal) nextWorkflowVersion(ctx context.Context, workflowID string) (int, error) {
	cur, err := j.CurrentWorkflowVersion(ctx, workflowID)
	if err != nil {
		return 0, err
	}
	return cur + 1, nil
}
