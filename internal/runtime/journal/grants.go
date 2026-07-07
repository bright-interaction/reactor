package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Grant is one row of workflow_secret_grants. Returned by ListGrants
// for the dashboard / `reactor vault grants` CLI surface.
type Grant struct {
	WorkflowID   string    `json:"workflow_id"`
	CredentialID string    `json:"credential_id"`
	GrantedAt    time.Time `json:"granted_at"`
	GrantedBy    string    `json:"granted_by,omitempty"`
	Note         string    `json:"note,omitempty"`
}

// GrantSecret records that a workflow may read a credential. Idempotent
// via INSERT OR REPLACE / ON CONFLICT semantics so repeated grants
// (e.g. CI re-applying a config) don't error.
func (j *Journal) GrantSecret(ctx context.Context, workflowID, credentialID, grantedBy, note string) error {
	if workflowID == "" || credentialID == "" {
		return errors.New("journal: grant: workflow_id and credential_id required")
	}
	// Cross-tenant refusal: a grant must never link a workflow to a credential
	// owned by a different tenant (that is the cross-tenant secret-disclosure
	// path, since HasGrant only checks the (workflow, credential) pair). Look
	// both up and refuse when both rows exist with differing tenants. When
	// either row is absent there is nothing to cross, so we don't force
	// existence here (the calling surface reports unknown ids).
	var wfTenant, credTenant sql.NullString
	_ = j.db.QueryRowContext(ctx, j.bind(`SELECT tenant_id FROM workflows WHERE id = $1`), workflowID).Scan(&wfTenant)
	_ = j.db.QueryRowContext(ctx, j.bind(`SELECT tenant_id FROM credentials WHERE id = $1 AND deleted_at IS NULL`), credentialID).Scan(&credTenant)
	if wfTenant.Valid && credTenant.Valid && wfTenant.String != credTenant.String {
		return fmt.Errorf("journal: grant refused: workflow tenant %q != credential tenant %q (cross-tenant grants are not allowed)", wfTenant.String, credTenant.String)
	}
	q := `INSERT INTO workflow_secret_grants (workflow_id, credential_id, granted_by, note)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workflow_id, credential_id) DO UPDATE SET
			granted_by = EXCLUDED.granted_by,
			note       = EXCLUDED.note`
	if j.engine == EngineSQLite {
		q = `INSERT INTO workflow_secret_grants (workflow_id, credential_id, granted_by, note)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (workflow_id, credential_id) DO UPDATE SET
				granted_by = excluded.granted_by,
				note       = excluded.note`
	}
	_, err := j.db.ExecContext(ctx, j.bind(q),
		workflowID, credentialID, nullable(grantedBy), nullable(note))
	if err != nil {
		return fmt.Errorf("journal: grant secret: %w", err)
	}
	return nil
}

// RevokeSecret removes a grant. Returns ErrNotFound if no row matched.
func (j *Journal) RevokeSecret(ctx context.Context, workflowID, credentialID string) error {
	const q = `DELETE FROM workflow_secret_grants
		WHERE workflow_id = $1 AND credential_id = $2`
	res, err := j.db.ExecContext(ctx, j.bind(q), workflowID, credentialID)
	if err != nil {
		return fmt.Errorf("journal: revoke secret: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// HasGrant reports whether a workflow can read a credential. Returns
// ErrACLEmpty when the table is empty (treated as v0 permissive mode);
// callers map this to "allow" to keep upgrades non-breaking.
func (j *Journal) HasGrant(ctx context.Context, workflowID, credentialID string) (bool, error) {
	const exist = `SELECT 1 FROM workflow_secret_grants LIMIT 1`
	var probe int
	if err := j.db.QueryRowContext(ctx, exist).Scan(&probe); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrACLEmpty
		}
		return false, fmt.Errorf("journal: probe grants: %w", err)
	}

	const q = `SELECT 1 FROM workflow_secret_grants
		WHERE workflow_id = $1 AND credential_id = $2 LIMIT 1`
	var ok int
	err := j.db.QueryRowContext(ctx, j.bind(q), workflowID, credentialID).Scan(&ok)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("journal: has grant: %w", err)
	}
	return true, nil
}

// ErrACLEmpty signals that the workflow_secret_grants table is empty
// and the caller should fall back to permissive mode. Lets fresh
// installs work without forcing an operator to seed grants for every
// existing workflow x credential pair.
var ErrACLEmpty = errors.New("journal: secret ACL empty (permissive)")

// ListGrants returns every grant ordered by workflow then credential.
// Used by the dashboard + `reactor vault grants list` CLI.
func (j *Journal) ListGrants(ctx context.Context) ([]Grant, error) {
	const q = `SELECT workflow_id, credential_id, granted_at, granted_by, note
		FROM workflow_secret_grants ORDER BY workflow_id, credential_id`
	rows, err := j.db.QueryContext(ctx, j.bind(q))
	if err != nil {
		return nil, fmt.Errorf("journal: list grants: %w", err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var (
			g           Grant
			granted     sql.NullString
			grantedBy   sql.NullString
			note        sql.NullString
		)
		if err := rows.Scan(&g.WorkflowID, &g.CredentialID, &granted, &grantedBy, &note); err != nil {
			return nil, fmt.Errorf("journal: scan grant: %w", err)
		}
		g.GrantedBy = nullableString(grantedBy)
		g.Note = nullableString(note)
		if granted.Valid {
			if t, err := j.parseTime(granted.String); err == nil {
				g.GrantedAt = t
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListGrantsForWorkflow narrows ListGrants to one workflow.
func (j *Journal) ListGrantsForWorkflow(ctx context.Context, workflowID string) ([]Grant, error) {
	const q = `SELECT workflow_id, credential_id, granted_at, granted_by, note
		FROM workflow_secret_grants WHERE workflow_id = $1 ORDER BY credential_id`
	rows, err := j.db.QueryContext(ctx, j.bind(q), workflowID)
	if err != nil {
		return nil, fmt.Errorf("journal: list grants by workflow: %w", err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var (
			g         Grant
			granted   sql.NullString
			grantedBy sql.NullString
			note      sql.NullString
		)
		if err := rows.Scan(&g.WorkflowID, &g.CredentialID, &granted, &grantedBy, &note); err != nil {
			return nil, err
		}
		g.GrantedBy = nullableString(grantedBy)
		g.Note = nullableString(note)
		if granted.Valid {
			if t, err := j.parseTime(granted.String); err == nil {
				g.GrantedAt = t
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
