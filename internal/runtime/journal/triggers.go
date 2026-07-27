package journal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// TriggerKind enumerates the supported trigger sources. Webhook + cron land
// in week 5; cdc + docker + manual ride along on the same table.
type TriggerKind string

const (
	TriggerWebhook          TriggerKind = "webhook"
	TriggerCron             TriggerKind = "cron"
	TriggerManual           TriggerKind = "manual"
	TriggerCDC              TriggerKind = "cdc"
	TriggerDocker           TriggerKind = "docker"
	TriggerWorkflowComplete TriggerKind = "workflow_complete"
)

// Trigger is a row from the triggers table. The webhook fields (TokenID,
// SecretID, Provider) are populated only for webhook triggers.
type Trigger struct {
	ID          string
	TenantID    string
	WorkflowID  string
	Kind        TriggerKind
	Config      []byte // JSON
	State       string
	TokenID     string
	SecretID    string
	Provider    string
	LastFiredAt *time.Time
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateWebhookTrigger registers a webhook binding. tokenID is the public
// URL component (POST /webhook/{tokenID}); the caller is responsible for
// generating one via NewTokenID. secretID points at a vault credential
// holding the HMAC shared secret.
func (j *Journal) CreateWebhookTrigger(ctx context.Context, workflowID, tokenID, secretID, provider string, config []byte) (string, error) {
	id, err := newID("trg_")
	if err != nil {
		return "", err
	}
	cfg := outputArg(config, j.engine)
	if cfg == nil {
		cfg = "{}"
		if j.engine != EngineSQLite {
			cfg = []byte("{}")
		}
	}
	const q = `INSERT INTO triggers
		(id, workflow_id, kind, config_json, state, token_id, secret_id, provider)
		VALUES ($1, $2, 'webhook', $3, 'active', $4, $5, $6)`
	_, err = j.db.ExecContext(ctx, j.bind(q),
		id, workflowID, cfg, tokenID, secretID, nullable(provider),
	)
	if err != nil {
		return "", fmt.Errorf("journal: create webhook trigger: %w", err)
	}
	return id, nil
}

// FindWebhookByToken resolves a public token to its trigger row. Returns
// ErrNotFound if no active webhook trigger exists with that token.
func (j *Journal) FindWebhookByToken(ctx context.Context, tokenID string) (Trigger, error) {
	const q = `SELECT id, tenant_id, workflow_id, kind, config_json, state,
		token_id, secret_id, provider, last_fired_at, last_error, created_at, updated_at
		FROM triggers WHERE token_id = $1 AND state = 'active' AND kind = 'webhook' LIMIT 1`
	row := j.db.QueryRowContext(ctx, j.bind(q), tokenID)
	return j.scanTrigger(row)
}

// MarkTriggerFired updates last_fired_at + clears last_error. Called on
// successful dispatch.
func (j *Journal) MarkTriggerFired(ctx context.Context, id string) error {
	const q = `UPDATE triggers SET last_fired_at = $1, last_error = NULL, updated_at = $2 WHERE id = $3`
	now := j.now()
	_, err := j.db.ExecContext(ctx, j.bind(q), now, now, id)
	return err
}

// SetTriggerState flips a trigger between 'active' and 'disabled'.
// Used by the cron live-reload + the webhook CLI's enable/disable
// subcommands so an operator can pause a trigger without deleting it.
func (j *Journal) SetTriggerState(ctx context.Context, id, state string) error {
	const q = `UPDATE triggers SET state = $1, updated_at = $2 WHERE id = $3`
	_, err := j.db.ExecContext(ctx, j.bind(q), state, j.now(), id)
	if err != nil {
		return fmt.Errorf("journal: set trigger state: %w", err)
	}
	return nil
}

// MarkTriggerError stores last_error for dashboard surfacing. Successful
// later fires clear it via MarkTriggerFired.
func (j *Journal) MarkTriggerError(ctx context.Context, id, msg string) error {
	const q = `UPDATE triggers SET last_error = $1, updated_at = $2 WHERE id = $3`
	_, err := j.db.ExecContext(ctx, j.bind(q), msg, j.now(), id)
	return err
}

// RecordWebhookDelivery inserts (provider, delivery_id) for dedup. Returns
// (true, nil) when the delivery is new, (false, nil) on duplicate. The
// caller treats duplicates as 200 no-ops per the webhook idempotency contract.
// triggerID scopes the dedup namespace. It used to be absent, so a delivery id
// consumed by ONE trigger made every other trigger receiving that id answer
// "deduped" and never dispatch. See migration 0025.
func (j *Journal) RecordWebhookDelivery(ctx context.Context, triggerID, provider, deliveryID string) (bool, error) {
	const q = `INSERT INTO webhook_deliveries (trigger_id, provider, delivery_id) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`
	res, err := j.db.ExecContext(ctx, j.bind(q), triggerID, provider, deliveryID)
	if err != nil {
		return false, fmt.Errorf("journal: record webhook delivery: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil // permissive: assume new if driver doesn't report
	}
	return n > 0, nil
}

// DeleteWebhookDelivery removes a dedup row. The webhook receiver calls
// this to roll back the dedup claim when dispatch fails, so the provider's
// retry of the SAME delivery is treated as fresh instead of being eaten
// as a duplicate. Best-effort: a missing row is not an error.
func (j *Journal) DeleteWebhookDelivery(ctx context.Context, triggerID, provider, deliveryID string) error {
	const q = `DELETE FROM webhook_deliveries WHERE trigger_id = $1 AND provider = $2 AND delivery_id = $3`
	if _, err := j.db.ExecContext(ctx, j.bind(q), triggerID, provider, deliveryID); err != nil {
		return fmt.Errorf("journal: delete webhook delivery: %w", err)
	}
	return nil
}

// ListTriggers returns every trigger row regardless of kind or state.
// Used by the graph builder so the AI Environment Lens sees every
// inbound source. Callers that need only active cron triggers should
// keep using ListCronTriggers.
func (j *Journal) ListTriggers(ctx context.Context) ([]Trigger, error) {
	const q = `SELECT id, tenant_id, workflow_id, kind, config_json, state,
		token_id, secret_id, provider, last_fired_at, last_error, created_at, updated_at
		FROM triggers ORDER BY created_at DESC`
	return j.scanTriggers(ctx, q)
}

// ListTriggersForWorkflow returns every trigger row bound to the given
// workflow id, newest first. Used by the dashboard's workflow detail
// page so an operator can see + manage inbound sources without dropping
// into SQL.
func (j *Journal) ListTriggersForWorkflow(ctx context.Context, workflowID string) ([]Trigger, error) {
	const q = `SELECT id, tenant_id, workflow_id, kind, config_json, state,
		token_id, secret_id, provider, last_fired_at, last_error, created_at, updated_at
		FROM triggers WHERE workflow_id = $1 ORDER BY created_at DESC`
	rows, err := j.db.QueryContext(ctx, j.bind(q), workflowID)
	if err != nil {
		return nil, fmt.Errorf("journal: list triggers by workflow: %w", err)
	}
	defer rows.Close()
	return scanTriggerRows(rows, j)
}

// DeleteTrigger removes a trigger row by id. Returns ErrNotFound when
// nothing matched so the dashboard surfaces a 404 rather than silently
// pretending success on a double-click.
func (j *Journal) DeleteTrigger(ctx context.Context, id string) error {
	const q = `DELETE FROM triggers WHERE id = $1`
	res, err := j.db.ExecContext(ctx, j.bind(q), id)
	if err != nil {
		return fmt.Errorf("journal: delete trigger: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCronTriggers returns every active cron trigger. Used by the cron
// driver at startup; cron-kind triggers added at runtime require a driver
// reload (week 9 wires LISTEN/NOTIFY for live reload on Postgres).
func (j *Journal) ListCronTriggers(ctx context.Context) ([]Trigger, error) {
	const q = `SELECT id, tenant_id, workflow_id, kind, config_json, state,
		token_id, secret_id, provider, last_fired_at, last_error, created_at, updated_at
		FROM triggers WHERE kind = 'cron' AND state = 'active'`
	return j.scanTriggers(ctx, q)
}

// scanTriggers is the shared body for ListTriggers + ListCronTriggers.
// Pulled out so adding a third filter doesn't trigger a fourth copy of
// the same scan loop.
func (j *Journal) scanTriggers(ctx context.Context, q string) ([]Trigger, error) {
	rows, err := j.db.QueryContext(ctx, j.bind(q))
	if err != nil {
		return nil, fmt.Errorf("journal: list triggers: %w", err)
	}
	defer rows.Close()
	return scanTriggerRows(rows, j)
}

// scanTriggerRows materialises Trigger structs from a *sql.Rows that
// returns the canonical 13-column SELECT in the order ListTriggers uses.
// Shared between ListTriggers / ListCronTriggers / ListTriggersForWorkflow.
func scanTriggerRows(rows *sql.Rows, j *Journal) ([]Trigger, error) {
	var out []Trigger
	for rows.Next() {
		var (
			t         Trigger
			tenantID  sql.NullString
			kind      string
			cfg       []byte
			tokenID   sql.NullString
			secretID  sql.NullString
			provider  sql.NullString
			lastFired sql.NullString
			lastErr   sql.NullString
			createdAt sql.NullString
			updatedAt sql.NullString
		)
		if err := rows.Scan(&t.ID, &tenantID, &t.WorkflowID, &kind, &cfg, &t.State,
			&tokenID, &secretID, &provider, &lastFired, &lastErr, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("journal: scan trigger: %w", err)
		}
		t.TenantID = nullableString(tenantID)
		t.Kind = TriggerKind(kind)
		t.Config = cfg
		t.TokenID = nullableString(tokenID)
		t.SecretID = nullableString(secretID)
		t.Provider = nullableString(provider)
		if lastFired.Valid {
			if ts, err := j.parseTime(lastFired.String); err == nil {
				t.LastFiredAt = &ts
			}
		}
		t.LastError = nullableString(lastErr)
		if createdAt.Valid {
			if ts, err := j.parseTime(createdAt.String); err == nil {
				t.CreatedAt = ts
			}
		}
		if updatedAt.Valid {
			if ts, err := j.parseTime(updatedAt.String); err == nil {
				t.UpdatedAt = ts
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateCronTrigger registers a cron-kind trigger. config is the JSON for
// {spec, timezone} that the cron driver reads at startup.
func (j *Journal) CreateCronTrigger(ctx context.Context, workflowID string, config []byte) (string, error) {
	id, err := newID("trg_")
	if err != nil {
		return "", err
	}
	cfg := outputArg(config, j.engine)
	if cfg == nil {
		cfg = "{}"
		if j.engine != EngineSQLite {
			cfg = []byte("{}")
		}
	}
	const q = `INSERT INTO triggers (id, workflow_id, kind, config_json, state)
		VALUES ($1, $2, 'cron', $3, 'active')`
	_, err = j.db.ExecContext(ctx, j.bind(q), id, workflowID, cfg)
	if err != nil {
		return "", fmt.Errorf("journal: create cron trigger: %w", err)
	}
	return id, nil
}

// PurgeOldWebhookDeliveries trims rows older than retain. Run periodically
// so the dedup table doesn't grow unbounded. Default retention 7 days.
func (j *Journal) PurgeOldWebhookDeliveries(ctx context.Context, retain time.Duration) error {
	cutoff := j.formatTime(time.Now().UTC().Add(-retain))
	const q = `DELETE FROM webhook_deliveries WHERE received_at < $1`
	_, err := j.db.ExecContext(ctx, j.bind(q), cutoff)
	return err
}

// NewTokenID generates a URL-safe webhook token.
func NewTokenID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("journal: token id: %w", err)
	}
	return "whk_" + hex.EncodeToString(b), nil
}

func (j *Journal) scanTrigger(row *sql.Row) (Trigger, error) {
	var (
		t         Trigger
		tenantID  sql.NullString
		kind      string
		cfg       []byte
		tokenID   sql.NullString
		secretID  sql.NullString
		provider  sql.NullString
		lastFired sql.NullString
		lastErr   sql.NullString
		createdAt sql.NullString
		updatedAt sql.NullString
	)
	if err := row.Scan(&t.ID, &tenantID, &t.WorkflowID, &kind, &cfg, &t.State,
		&tokenID, &secretID, &provider, &lastFired, &lastErr, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Trigger{}, ErrNotFound
		}
		return Trigger{}, fmt.Errorf("journal: scan trigger: %w", err)
	}
	t.TenantID = nullableString(tenantID)
	t.Kind = TriggerKind(kind)
	t.Config = cfg
	t.TokenID = nullableString(tokenID)
	t.SecretID = nullableString(secretID)
	t.Provider = nullableString(provider)
	if lastFired.Valid {
		if ts, err := j.parseTime(lastFired.String); err == nil {
			t.LastFiredAt = &ts
		}
	}
	t.LastError = nullableString(lastErr)
	if createdAt.Valid {
		if ts, err := j.parseTime(createdAt.String); err == nil {
			t.CreatedAt = ts
		}
	}
	if updatedAt.Valid {
		if ts, err := j.parseTime(updatedAt.String); err == nil {
			t.UpdatedAt = ts
		}
	}
	return t, nil
}

func nullableString(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}
