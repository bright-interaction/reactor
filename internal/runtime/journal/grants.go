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

// ErrGrantTarget is returned when a grant or revoke names a workflow or
// credential that does not exist. Callers map this to HTTP 404 / a clear
// "unknown id" message rather than silently writing a phantom grant row.
var ErrGrantTarget = errors.New("journal: grant target does not exist")

// GrantSecret records that a workflow may read a credential. Idempotent
// via INSERT OR REPLACE / ON CONFLICT semantics so repeated grants
// (e.g. CI re-applying a config) don't error.
func (j *Journal) GrantSecret(ctx context.Context, workflowID, credentialID, grantedBy, note string) error {
	if workflowID == "" || credentialID == "" {
		return errors.New("journal: grant: workflow_id and credential_id required")
	}
	// Cross-tenant refusal: a grant must never link a workflow to a credential
	// owned by a different tenant, since HasGrant only checks the
	// (workflow, credential) pair and the grant table carries no tenant column.
	//
	// This used to skip the check whenever EITHER row was absent, on the stated
	// assumption that "the calling surface reports unknown ids". No surface
	// does: the MCP grant_secret tool passes both ids straight through, so
	// granting to a workflow id that does not exist returned ok:true and left a
	// phantom row (verified against a live stdio server), and `reactor vault
	// grant` accepted a nonexistent credential the same way. Requiring both
	// rows to resolve is what makes the tenant comparison meaningful.
	wfTenant, credTenant, err := j.grantTenants(ctx, workflowID, credentialID)
	if err != nil {
		return err
	}
	if wfTenant != credTenant {
		return fmt.Errorf("journal: grant refused: workflow tenant %q != credential tenant %q (cross-tenant grants are not allowed)", wfTenant, credTenant)
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
	if _, err := j.db.ExecContext(ctx, j.bind(q),
		workflowID, credentialID, nullable(grantedBy), nullable(note)); err != nil {
		return fmt.Errorf("journal: grant secret: %w", err)
	}
	return nil
}

// grantTenants resolves the owning tenant of both sides of a grant. Both rows
// must exist: a grant naming a resource that is not there is always a caller
// bug, and silently accepting it is what let phantom grants through.
func (j *Journal) grantTenants(ctx context.Context, workflowID, credentialID string) (string, string, error) {
	var wfTenant, credTenant sql.NullString
	err := j.db.QueryRowContext(ctx,
		j.bind(`SELECT tenant_id FROM workflows WHERE id = $1`), workflowID).Scan(&wfTenant)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: workflow %q", ErrGrantTarget, workflowID)
	}
	if err != nil {
		return "", "", fmt.Errorf("journal: grant: lookup workflow tenant: %w", err)
	}
	err = j.db.QueryRowContext(ctx,
		j.bind(`SELECT tenant_id FROM credentials WHERE id = $1 AND deleted_at IS NULL`), credentialID).Scan(&credTenant)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: credential %q", ErrGrantTarget, credentialID)
	}
	if err != nil {
		return "", "", fmt.Errorf("journal: grant: lookup credential tenant: %w", err)
	}
	return wfTenant.String, credTenant.String, nil
}

// RevokeSecret removes a grant. Returns ErrNotFound if no row matched.
func (j *Journal) RevokeSecret(ctx context.Context, workflowID, credentialID string) error {
	// Mirror GrantSecret's cross-tenant refusal. The 2026-07-07 fix pass added
	// it to grant and not to its inverse, so revoke was reachable across the
	// tenant boundary on all three surfaces (REST, MCP, CLI) and is a
	// cross-tenant denial of service: drop another tenant's grant and their
	// workflows start failing every secret fetch.
	wfTenant, credTenant, err := j.grantTenants(ctx, workflowID, credentialID)
	if err != nil {
		return err
	}
	if wfTenant != credTenant {
		return fmt.Errorf("journal: revoke refused: workflow tenant %q != credential tenant %q", wfTenant, credTenant)
	}
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
			g         Grant
			granted   sql.NullString
			grantedBy sql.NullString
			note      sql.NullString
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

// RunIdentity is the authoritative answer to "which workflow is this run, and
// whose is it". It exists because the supervisor previously answered that
// question from its WorkflowSlug via the UNSCOPED WorkflowIDBySlug, and slugs
// are unique only PER TENANT: with two tenants owning one slug, that resolved to
// whichever tenant registered it most recently and the secret ACL was then
// evaluated against the wrong workflow. A run id cannot be ambiguous that way.
//
// Fails with ErrNotFound rather than an empty string when the run is unknown, so
// callers cannot mistake "no identity" for "the default tenant".
func (j *Journal) RunIdentity(ctx context.Context, runID string) (workflowID, tenantID string, err error) {
	if runID == "" {
		return "", "", fmt.Errorf("journal: run identity: run id required")
	}
	var wf, tenant sql.NullString
	q := `SELECT r.workflow_id, COALESCE(w.tenant_id, r.tenant_id)
		FROM runs r LEFT JOIN workflows w ON w.id = r.workflow_id
		WHERE r.id = $1`
	switch err := j.db.QueryRowContext(ctx, j.bind(q), runID).Scan(&wf, &tenant); {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", ErrNotFound
	case err != nil:
		return "", "", fmt.Errorf("journal: run identity: %w", err)
	}
	return wf.String, tenant.String, nil
}

// CredentialTenant returns the owning tenant of a live credential.
// ErrNotFound for an unknown or soft-deleted id.
func (j *Journal) CredentialTenant(ctx context.Context, credentialID string) (string, error) {
	if credentialID == "" {
		return "", fmt.Errorf("journal: credential tenant: id required")
	}
	var tenant sql.NullString
	const q = `SELECT tenant_id FROM credentials WHERE id = $1 AND deleted_at IS NULL`
	switch err := j.db.QueryRowContext(ctx, j.bind(q), credentialID).Scan(&tenant); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("journal: credential tenant: %w", err)
	}
	return tenant.String, nil
}
