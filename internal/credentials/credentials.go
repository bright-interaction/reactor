// Package credentials owns the SQL helpers for the credentials + audit
// tables. Separate from the workflow journal because credentials are
// orthogonal to workflow runs and have their own lifecycle (rotation,
// audit, ACL).
//
// The vault.Store handles the encrypted blob; this package holds the
// metadata (provider, rotation interval, last-rotated, targets) plus
// the append-only audit log.
package credentials

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Engine reports the SQL dialect; the repo uses this to pick the right
// timestamp + jsonb representation. Mirrors journal.Engine.
type Engine string

const (
	EngineSQLite   Engine = "sqlite"
	EnginePostgres Engine = "postgres"
)

// ErrNotFound is returned when a credential id has no matching row.
var ErrNotFound = errors.New("credentials: not found")

// Repo is the queries handle.
type Repo struct {
	db     *sql.DB
	engine Engine
}

// New wraps an open *sql.DB.
func New(db *sql.DB, engine Engine) *Repo {
	return &Repo{db: db, engine: engine}
}

// Credential is the metadata-only view of a row. The encrypted blob is
// owned by vault.Store; metadata is what the rotation engine, dashboard,
// and MCP need.
type Credential struct {
	ID                   string            `json:"id"`
	TenantID             string            `json:"tenant_id"`
	Name                 string            `json:"name"`
	Service              string            `json:"service"`
	Provider             string            `json:"provider"`
	ProviderMeta         map[string]string `json:"provider_meta"`
	RotationPolicy       string            `json:"rotation_policy"`
	AutoRotate           bool              `json:"auto_rotate"`
	RotationIntervalDays int               `json:"rotation_interval_days"`
	LastRotatedAt        time.Time         `json:"last_rotated_at,omitempty"`
	LastRotationError    string            `json:"last_rotation_error,omitempty"`
	RotationTargets      []Target          `json:"rotation_targets"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

// Target is one delivery destination for a rotated credential. The
// rotator runs each target sequentially after the new value is encrypted
// + persisted.
type Target struct {
	Kind         string `json:"kind"`        // "webhook"
	URL          string `json:"url"`         // POST URL
	SecretID     string `json:"secret_id"`   // vault credential id holding HMAC secret
	KeyName      string `json:"key_name"`    // body key (e.g. SCANNER_SVAR_SECRET)
	GraceSeconds int    `json:"grace_seconds,omitempty"`
}

// TargetKinds are the delivery kinds the rotation runner implements, with the
// operator-facing meaning of the shared URL/KeyName fields (the Target struct is
// uniform; only the semantics differ per kind).
var TargetKinds = []struct {
	Kind, Label, URLHint, KeyNameHint string
	NeedsKeyName, NeedsSecretID       bool
}{
	{"webhook", "Webhook (one POST, receiver swaps atomically)", "POST URL the receiver listens on", "body key the new value arrives under, e.g. SCANNER_SVAR_SECRET", true, true},
	{"reload_endpoint", "Reload endpoint (two-phase: accept, then cut over)", "POST URL the receiver listens on", "body key the new value arrives under", true, true},
	{"file_write", "File write (drop the value on disk)", "absolute file path (must sit under REACTOR_FILE_WRITE_ROOT)", "", false, false},
	{"forgejo_secret", "Forgejo repo/org secret", "Forgejo API base for the repo or org", "secret name to set", true, true},
	{"github_secret", "GitHub Actions secret", "GitHub repo API base", "secret name to set", true, true},
	{"dockyard_vault", "Dockyard vault entry", "vault entry endpoint", "", false, true},
}

// Validate reports whether a target carries the fields its kind requires. It is
// the single source of truth for that rule: the dashboard uses it to reject a
// half-filled form, and the rotation runner uses it before attempting delivery,
// so the two cannot drift into accepting different things.
func (t Target) Validate() error {
	for _, k := range TargetKinds {
		if k.Kind != t.Kind {
			continue
		}
		if t.URL == "" {
			return fmt.Errorf("target %q missing url (%s)", t.Kind, k.URLHint)
		}
		if k.NeedsKeyName && t.KeyName == "" {
			return fmt.Errorf("target %q missing key_name (%s)", t.Kind, k.KeyNameHint)
		}
		if k.NeedsSecretID && t.SecretID == "" {
			return fmt.Errorf("target %q missing secret_id (the vault credential holding the auth secret for this target)", t.Kind)
		}
		if t.GraceSeconds < 0 {
			return fmt.Errorf("target %q grace_seconds must not be negative", t.Kind)
		}
		return nil
	}
	return fmt.Errorf("unsupported target kind %q", t.Kind)
}

// AuditEntry is a single row from credential_audit.
type AuditEntry struct {
	ID           int64           `json:"id"`
	CredentialID string          `json:"credential_id"`
	Action       string          `json:"action"`
	ActorKind    string          `json:"actor_kind"`
	ActorID      string          `json:"actor_id,omitempty"`
	WorkflowID   string          `json:"workflow_id,omitempty"`
	RunID        string          `json:"run_id,omitempty"`
	StepID       string          `json:"step_id,omitempty"`
	Detail       json.RawMessage `json:"detail"`
	At           time.Time       `json:"at"`
}

// CreateParams captures the rotation-aware fields the test seed + admin
// CLI pass when registering a credential.
type CreateParams struct {
	ID                   string
	Name                 string
	Service              string
	Provider             string
	ProviderMeta         map[string]string
	AutoRotate           bool
	RotationIntervalDays int
	RotationTargets      []Target
}

// Create inserts a credentials row with rotation metadata. The
// encrypted blob lives in vault.Store; we persist a sentinel here so
// the row's NOT NULL constraint is satisfied. Real deployments populate
// `blob` via vault.Store.Put which encrypts with the master key; tests
// can either go through vault or stash a sentinel.
func (r *Repo) Create(ctx context.Context, p CreateParams) error {
	if p.ID == "" || p.Name == "" {
		return errors.New("credentials: id and name required")
	}
	if p.Provider == "" {
		p.Provider = "manual"
	}
	meta := encodeJSON(p.ProviderMeta)
	targets := encodeJSON(p.RotationTargets)
	const q = `INSERT INTO credentials
		(id, tenant_id, name, service, blob, metadata,
		 provider, provider_meta, auto_rotate, rotation_interval_days, rotation_targets)
		VALUES ($1, 'default', $2, $3, $4, '{}', $5, $6, $7, $8, $9)`
	_, err := r.db.ExecContext(ctx, r.bind(q),
		p.ID, p.Name, p.Service,
		[]byte("sentinel"),
		p.Provider, meta, r.boolValue(p.AutoRotate), p.RotationIntervalDays, targets,
	)
	if err != nil {
		return fmt.Errorf("credentials: create: %w", err)
	}
	return nil
}

// Get returns a credential row by id.
func (r *Repo) Get(ctx context.Context, id string) (Credential, error) {
	const q = `SELECT id, tenant_id, name, service, provider, provider_meta,
		rotation_policy, auto_rotate, rotation_interval_days,
		last_rotated_at, last_rotation_error, rotation_targets, created_at, updated_at
		FROM credentials WHERE id = $1 AND deleted_at IS NULL`
	row := r.db.QueryRowContext(ctx, r.bind(q), id)
	c, err := r.scanCredential(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, ErrNotFound
		}
		return Credential{}, err
	}
	return c, nil
}

// List returns every non-deleted credential, name-sorted for stable CLI output.
func (r *Repo) List(ctx context.Context) ([]Credential, error) {
	const q = `SELECT id, tenant_id, name, service, provider, provider_meta,
		rotation_policy, auto_rotate, rotation_interval_days,
		last_rotated_at, last_rotation_error, rotation_targets, created_at, updated_at
		FROM credentials WHERE deleted_at IS NULL ORDER BY name ASC`
	rows, err := r.db.QueryContext(ctx, r.bind(q))
	if err != nil {
		return nil, fmt.Errorf("credentials: list: %w", err)
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := r.scanCredential(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListNeedingRotation returns credentials whose auto_rotate is true and
// whose last_rotated_at is past the configured interval (or never). Used
// by the rotation runner's tick loop.
func (r *Repo) ListNeedingRotation(ctx context.Context, now time.Time) ([]Credential, error) {
	const q = `SELECT id, tenant_id, name, service, provider, provider_meta,
		rotation_policy, auto_rotate, rotation_interval_days,
		last_rotated_at, last_rotation_error, rotation_targets, created_at, updated_at
		FROM credentials WHERE deleted_at IS NULL AND auto_rotate = $1`
	rows, err := r.db.QueryContext(ctx, r.bind(q), r.boolValue(true))
	if err != nil {
		return nil, fmt.Errorf("credentials: list rotation: %w", err)
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := r.scanCredential(rows.Scan)
		if err != nil {
			return nil, err
		}
		// Filter in Go: cross-engine arithmetic on dates is awkward in
		// portable SQL. The list is small enough (< thousands of creds
		// per FF deployment) that this is fine.
		if c.LastRotatedAt.IsZero() {
			out = append(out, c)
			continue
		}
		due := c.LastRotatedAt.Add(time.Duration(c.RotationIntervalDays) * 24 * time.Hour)
		if !due.After(now) {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// MarkRotated updates last_rotated_at + clears last_rotation_error.
// SetRotationTargets replaces a credential's delivery targets.
//
// Before this existed, rotation_targets could only be populated by hand-written
// SQL: no repo setter, no caller, no UI, no CLI flag. So every credential an
// operator could create had ZERO targets, and rotation stored the new value and
// delivered it nowhere while the audit trail recorded success. Delivery is the
// product's headline differentiator, so it was the differentiator that was
// unreachable.
//
// Every target is validated before the write, so a malformed one cannot sit in
// the column waiting to fail at rotation time (hours later, in a scheduler tick
// no one is watching).
func (r *Repo) SetRotationTargets(ctx context.Context, id string, targets []Target) error {
	for i, t := range targets {
		if err := t.Validate(); err != nil {
			return fmt.Errorf("credentials: target %d: %w", i+1, err)
		}
	}
	if targets == nil {
		targets = []Target{}
	}
	const q = `UPDATE credentials SET rotation_targets = $1, updated_at = $2 WHERE id = $3`
	res, err := r.db.ExecContext(ctx, r.bind(q), encodeJSON(targets), r.formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("credentials: set rotation targets: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) MarkRotated(ctx context.Context, id string, at time.Time) error {
	const q = `UPDATE credentials
		SET last_rotated_at = $1, last_rotation_error = NULL, updated_at = $2
		WHERE id = $3`
	res, err := r.db.ExecContext(ctx, r.bind(q), r.formatTime(at), r.formatTime(at), id)
	if err != nil {
		return fmt.Errorf("credentials: mark rotated: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordError stamps last_rotation_error so the dashboard / CLI can
// surface "this credential failed last rotation". Successful later
// rotations clear it via MarkRotated.
func (r *Repo) RecordError(ctx context.Context, id, msg string) error {
	const q = `UPDATE credentials SET last_rotation_error = $1, updated_at = $2 WHERE id = $3`
	now := r.formatTime(time.Now().UTC())
	_, err := r.db.ExecContext(ctx, r.bind(q), msg, now, id)
	return err
}

// AppendAudit writes a row to credential_audit. detail is a free-form
// JSON map describing the event (target URL, error text, etc.).
func (r *Repo) AppendAudit(ctx context.Context, e AuditEntry) error {
	if e.CredentialID == "" || e.Action == "" || e.ActorKind == "" {
		return errors.New("credentials: audit credential_id, action, actor_kind required")
	}
	detail := e.Detail
	if len(detail) == 0 {
		detail = json.RawMessage("{}")
	}
	d := outputArg(detail, r.engine)
	const q = `INSERT INTO credential_audit
		(credential_id, action, actor_kind, actor_id, workflow_id, run_id, step_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := r.db.ExecContext(ctx, r.bind(q),
		e.CredentialID, e.Action, e.ActorKind,
		nullable(e.ActorID), nullable(e.WorkflowID),
		nullable(e.RunID), nullable(e.StepID), d,
	)
	if err != nil {
		return fmt.Errorf("credentials: audit append: %w", err)
	}
	return nil
}

// ListAudit returns the most recent audit entries for a credential,
// newest first. limit caps the result; 0 means default 50.
func (r *Repo) ListAudit(ctx context.Context, credentialID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	const q = `SELECT id, credential_id, action, actor_kind, actor_id, workflow_id, run_id, step_id, detail, at
		FROM credential_audit WHERE credential_id = $1 ORDER BY at DESC, id DESC LIMIT $2`
	rows, err := r.db.QueryContext(ctx, r.bind(q), credentialID, limit)
	if err != nil {
		return nil, fmt.Errorf("credentials: audit list: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var (
			e          AuditEntry
			actorID    sql.NullString
			workflowID sql.NullString
			runID      sql.NullString
			stepID     sql.NullString
			detail     []byte
			at         sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.CredentialID, &e.Action, &e.ActorKind,
			&actorID, &workflowID, &runID, &stepID, &detail, &at); err != nil {
			return nil, fmt.Errorf("credentials: audit scan: %w", err)
		}
		e.ActorID = nullableString(actorID)
		e.WorkflowID = nullableString(workflowID)
		e.RunID = nullableString(runID)
		e.StepID = nullableString(stepID)
		if len(detail) > 0 {
			e.Detail = append(json.RawMessage(nil), detail...)
		} else {
			e.Detail = json.RawMessage("{}")
		}
		if at.Valid {
			if t, err := r.parseTime(at.String); err == nil {
				e.At = t
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanCredential is shared by Get / List / ListNeedingRotation.
func (r *Repo) scanCredential(scan func(...any) error) (Credential, error) {
	var (
		c            Credential
		tenantID     sql.NullString
		providerMeta []byte
		policy       sql.NullString
		autoRotate   any
		intervalDays sql.NullInt64
		lastRotated  sql.NullString
		lastErr      sql.NullString
		targets      []byte
		createdAt    sql.NullString
		updatedAt    sql.NullString
	)
	if err := scan(&c.ID, &tenantID, &c.Name, &c.Service, &c.Provider, &providerMeta,
		&policy, &autoRotate, &intervalDays, &lastRotated, &lastErr, &targets, &createdAt, &updatedAt); err != nil {
		return Credential{}, err
	}
	c.TenantID = nullableString(tenantID)
	c.RotationPolicy = nullableString(policy)
	c.AutoRotate = parseBool(autoRotate)
	c.RotationIntervalDays = int(intervalDays.Int64)
	c.LastRotationError = nullableString(lastErr)
	c.ProviderMeta = decodeStringMap(providerMeta)
	c.RotationTargets = decodeTargets(targets)
	if lastRotated.Valid {
		if t, err := r.parseTime(lastRotated.String); err == nil {
			c.LastRotatedAt = t
		}
	}
	if createdAt.Valid {
		if t, err := r.parseTime(createdAt.String); err == nil {
			c.CreatedAt = t
		}
	}
	if updatedAt.Valid {
		if t, err := r.parseTime(updatedAt.String); err == nil {
			c.UpdatedAt = t
		}
	}
	return c, nil
}

// NewID generates an opaque credential id with a "cred_" prefix.
func NewID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("credentials: id: %w", err)
	}
	return "cred_" + hex.EncodeToString(b), nil
}

// --- internal helpers ---

func (r *Repo) bind(q string) string {
	if r.engine != EngineSQLite {
		return q
	}
	out := make([]byte, 0, len(q))
	for i := 0; i < len(q); i++ {
		if q[i] == '$' && i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9' {
			out = append(out, '?')
			i++
			for i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9' {
				i++
			}
			continue
		}
		out = append(out, q[i])
	}
	return string(out)
}

func (r *Repo) formatTime(t time.Time) any {
	if r.engine == EngineSQLite {
		return t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return t.UTC()
}

func (r *Repo) parseTime(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("credentials: unparseable time %q", s)
}

func (r *Repo) boolValue(b bool) any {
	if r.engine == EngineSQLite {
		if b {
			return 1
		}
		return 0
	}
	return b
}

func parseBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case []byte:
		return len(x) == 1 && x[0] == 't'
	case string:
		return x == "t" || x == "true" || x == "1"
	}
	return false
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableString(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

func encodeJSON(v any) any {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return "{}"
	}
	return string(b)
}

func decodeStringMap(raw []byte) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	m := map[string]string{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func decodeTargets(raw []byte) []Target {
	if len(raw) == 0 {
		return nil
	}
	var out []Target
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func outputArg(out json.RawMessage, engine Engine) any {
	if len(out) == 0 {
		return nil
	}
	if engine == EngineSQLite {
		return string(out)
	}
	return []byte(out)
}
