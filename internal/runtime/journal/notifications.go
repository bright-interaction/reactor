package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Notification channel kinds.
const (
	ChannelKindSlackWebhook   = "slack_webhook"
	ChannelKindGenericWebhook = "generic_webhook"
	ChannelKindEmailSMTP      = "email_smtp"
)

// NotificationChannel is one configured destination an operator wants
// to receive run-status alerts on. config_json shape varies by kind;
// the notifier package owns the per-kind unmarshalling.
type NotificationChannel struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	ConfigJSON json.RawMessage `json:"config_json"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// WorkflowNotificationRoute pairs a workflow with a channel + the list
// of terminal statuses (comma-separated) that should fire it.
type WorkflowNotificationRoute struct {
	WorkflowID string    `json:"workflow_id"`
	ChannelID  string    `json:"channel_id"`
	OnStatuses string    `json:"on_statuses"`
	CreatedAt  time.Time `json:"created_at"`
}

// ErrChannelInUse is returned by DeleteNotificationChannel when at
// least one workflow_notification_routes row still references the
// channel. Operator-side message: detach the routes first.
var ErrChannelInUse = errors.New("journal: notification channel has active routes")

// ErrChannelNameTaken means the tenant already owns a channel by that name.
// It is scoped per tenant (migration 0027), so this can only ever be a
// collision inside the caller's own tenant. It exists so the handler can render
// something an operator can act on instead of the raw driver text: reporting
// "UNIQUE constraint failed: notification_channels.tenant_id, ..." to a browser
// both leaks schema and tells the user nothing about what to do.
var ErrChannelNameTaken = errors.New("journal: a notification channel with that name already exists")

// CreateNotificationChannel inserts a row + returns the generated id.
// (tenant_id, name) is UNIQUE, so re-creating the same name within one tenant
// returns ErrChannelNameTaken while a different tenant may reuse it freely.
func (j *Journal) CreateNotificationChannel(ctx context.Context, name, kind string, config json.RawMessage) (string, error) {
	id, err := newID("nch_")
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", errors.New("journal: notification channel name is required")
	}
	if !validChannelKind(kind) {
		return "", fmt.Errorf("journal: unknown channel kind %q", kind)
	}
	if !json.Valid(config) {
		return "", errors.New("journal: notification channel config must be valid JSON")
	}
	return j.createChannel(ctx, id, DefaultTenant, name, kind, config)
}

// CreateNotificationChannelInTenant records the owning tenant. A channel is a
// top-level entity reachable only by id, so without this it had no tenant
// anchor at all and every list returned every channel in the install.
func (j *Journal) CreateNotificationChannelInTenant(ctx context.Context, tenantID, name, kind string, config json.RawMessage) (string, error) {
	id, err := newID("nch_")
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", errors.New("journal: notification channel name is required")
	}
	if !validChannelKind(kind) {
		return "", fmt.Errorf("journal: unknown channel kind %q", kind)
	}
	if !json.Valid(config) {
		return "", errors.New("journal: notification channel config must be valid JSON")
	}
	if tenantID == "" {
		tenantID = DefaultTenant
	}
	return j.createChannel(ctx, id, tenantID, name, kind, config)
}

func (j *Journal) createChannel(ctx context.Context, id, tenantID, name, kind string, config json.RawMessage) (string, error) {
	const q = `INSERT INTO notification_channels (id, tenant_id, name, kind, config_json) VALUES ($1, $2, $3, $4, $5)`
	cfg := outputArg(config, j.engine)
	if _, err := j.db.ExecContext(ctx, j.bind(q), id, tenantID, name, kind, cfg); err != nil {
		if isUniqueViolation(err) {
			return "", ErrChannelNameTaken
		}
		return "", fmt.Errorf("journal: create notification channel: %w", err)
	}
	return id, nil
}

// isUniqueViolation probes a driver error for a uniqueness conflict. sqlite says
// "UNIQUE constraint failed", postgres says "duplicate key value violates unique
// constraint", so sniff both rather than binding to either driver's error type.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}

func validChannelKind(kind string) bool {
	switch kind {
	case ChannelKindSlackWebhook, ChannelKindGenericWebhook, ChannelKindEmailSMTP:
		return true
	}
	return false
}

// ListNotificationChannels returns every channel ordered by name.
func (j *Journal) ListNotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	return j.ListNotificationChannelsByTenant(ctx, "")
}

// ListNotificationChannelsByTenant scopes the list. An empty tenantID means
// every channel, which is correct for a global admin and wrong for anyone else:
// the member-facing workflow detail page builds its attach-channel <select>
// from this, so an unscoped call there leaks the id/name/kind of every channel
// in the install.
func (j *Journal) ListNotificationChannelsByTenant(ctx context.Context, tenantID string) ([]NotificationChannel, error) {
	q := `SELECT id, tenant_id, name, kind, config_json, created_at, updated_at
		FROM notification_channels`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id = $1`
		args = append(args, tenantID)
	}
	q += ` ORDER BY name ASC`
	rows, err := j.db.QueryContext(ctx, j.bind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("journal: list notification channels: %w", err)
	}
	defer rows.Close()
	var out []NotificationChannel
	for rows.Next() {
		ch, err := scanChannel(rows.Scan, j)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// GetNotificationChannel returns one channel by id.
func (j *Journal) GetNotificationChannel(ctx context.Context, id string) (NotificationChannel, error) {
	const q = `SELECT id, tenant_id, name, kind, config_json, created_at, updated_at
		FROM notification_channels WHERE id = $1`
	row := j.db.QueryRowContext(ctx, j.bind(q), id)
	ch, err := scanChannel(row.Scan, j)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NotificationChannel{}, ErrNotFound
		}
		return NotificationChannel{}, err
	}
	return ch, nil
}

// DeleteNotificationChannel removes a channel. Refuses to proceed when
// any workflow_notification_routes row still references it; operator
// is expected to drop the routes first (or use a cascade if they
// really want to).
func (j *Journal) DeleteNotificationChannel(ctx context.Context, id string) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, j.bind(`SELECT COUNT(*) FROM workflow_notification_routes WHERE channel_id = $1`), id).Scan(&n); err != nil {
		return fmt.Errorf("journal: probe routes: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %d workflow(s) routed", ErrChannelInUse, n)
	}
	res, err := tx.ExecContext(ctx, j.bind(`DELETE FROM notification_channels WHERE id = $1`), id)
	if err != nil {
		return fmt.Errorf("journal: delete notification channel: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// AddNotificationRoute upserts a (workflow_id, channel_id) row with
// the given on_statuses list. Re-adding the same pair replaces the
// trigger statuses so an operator can broaden / narrow without
// dropping the route first.
func (j *Journal) AddNotificationRoute(ctx context.Context, workflowID, channelID, onStatuses string) error {
	if workflowID == "" || channelID == "" {
		return errors.New("journal: workflow_id and channel_id are required")
	}
	onStatuses = normaliseStatuses(onStatuses)
	if onStatuses == "" {
		return errors.New("journal: on_statuses must list at least one terminal status")
	}
	// Cross-tenant refusal, same shape as GrantSecret. A route is a relation
	// between two owned things, and until migration 0026 gave channels a
	// tenant_id this comparison was impossible, so it was never written: an admin
	// could point tenant A's workflow at tenant B's channel and every failure of
	// A's run (slug, error text, dashboard URL) would be delivered into B's Slack.
	// Requiring both rows to resolve is what makes the comparison meaningful,
	// otherwise a route naming a nonexistent channel silently "succeeds".
	wfTenant, chTenant, err := j.routeTenants(ctx, workflowID, channelID)
	if err != nil {
		return err
	}
	if wfTenant != chTenant {
		return fmt.Errorf("journal: notification route refused: workflow tenant %q != channel tenant %q (cross-tenant routes are not allowed)", wfTenant, chTenant)
	}
	const q = `INSERT INTO workflow_notification_routes (workflow_id, channel_id, on_statuses)
		VALUES ($1, $2, $3)
		ON CONFLICT(workflow_id, channel_id) DO UPDATE SET on_statuses = excluded.on_statuses`
	if _, err := j.db.ExecContext(ctx, j.bind(q), workflowID, channelID, onStatuses); err != nil {
		return fmt.Errorf("journal: add notification route: %w", err)
	}
	return nil
}

// normaliseStatuses trims + lowercases + dedupes + sorts so two
// route inserts with the same intended set produce identical rows.
func normaliseStatuses(s string) string {
	parts := strings.Split(s, ",")
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	// Stable order so equality comparisons round-trip.
	return strings.Join(sortStrings(out), ",")
}

func sortStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// DeleteNotificationRoute drops the (workflow_id, channel_id) pair.
func (j *Journal) DeleteNotificationRoute(ctx context.Context, workflowID, channelID string) error {
	const q = `DELETE FROM workflow_notification_routes WHERE workflow_id = $1 AND channel_id = $2`
	res, err := j.db.ExecContext(ctx, j.bind(q), workflowID, channelID)
	if err != nil {
		return fmt.Errorf("journal: delete notification route: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// ListNotificationRoutesForWorkflow returns every routed channel for
// a workflow plus the joined channel row so the dashboard can render
// channel name + kind without a second lookup.
type NotificationRouteWithChannel struct {
	WorkflowID  string
	ChannelID   string
	ChannelName string
	ChannelKind string
	OnStatuses  string
	CreatedAt   time.Time
}

func (j *Journal) ListNotificationRoutesForWorkflow(ctx context.Context, workflowID string) ([]NotificationRouteWithChannel, error) {
	const q = `SELECT r.workflow_id, r.channel_id, c.name, c.kind, r.on_statuses, r.created_at
		FROM workflow_notification_routes r
		JOIN notification_channels c ON c.id = r.channel_id
		WHERE r.workflow_id = $1
		ORDER BY c.name ASC`
	rows, err := j.db.QueryContext(ctx, j.bind(q), workflowID)
	if err != nil {
		return nil, fmt.Errorf("journal: list routes for workflow: %w", err)
	}
	defer rows.Close()
	var out []NotificationRouteWithChannel
	for rows.Next() {
		var (
			row     NotificationRouteWithChannel
			created sql.NullString
		)
		if err := rows.Scan(&row.WorkflowID, &row.ChannelID, &row.ChannelName, &row.ChannelKind, &row.OnStatuses, &created); err != nil {
			return nil, err
		}
		if created.Valid {
			if t, perr := j.parseTime(created.String); perr == nil {
				row.CreatedAt = t
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ChannelsForRunTerminal returns the channels that should fire for a
// terminal run, deduplicated and with their config_json so the
// notifier can dispatch without a second round-trip.
//
// status comparison is exact (lowercased) against each route's
// on_statuses CSV. Dispatcher calls this from OnTerminal.
func (j *Journal) ChannelsForRunTerminal(ctx context.Context, workflowID, status string) ([]NotificationChannel, error) {
	const q = `SELECT c.id, c.name, c.kind, c.config_json, c.created_at, c.updated_at, r.on_statuses
		FROM workflow_notification_routes r
		JOIN notification_channels c ON c.id = r.channel_id
		WHERE r.workflow_id = $1`
	rows, err := j.db.QueryContext(ctx, j.bind(q), workflowID)
	if err != nil {
		return nil, fmt.Errorf("journal: channels for run terminal: %w", err)
	}
	defer rows.Close()
	status = strings.ToLower(strings.TrimSpace(status))
	var out []NotificationChannel
	for rows.Next() {
		var (
			ch         NotificationChannel
			cfgBytes   []byte
			created    sql.NullString
			updated    sql.NullString
			onStatuses string
		)
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Kind, &cfgBytes, &created, &updated, &onStatuses); err != nil {
			return nil, err
		}
		if !routeFiresOn(onStatuses, status) {
			continue
		}
		ch.ConfigJSON = append(json.RawMessage(nil), cfgBytes...)
		if created.Valid {
			if t, perr := j.parseTime(created.String); perr == nil {
				ch.CreatedAt = t
			}
		}
		if updated.Valid {
			if t, perr := j.parseTime(updated.String); perr == nil {
				ch.UpdatedAt = t
			}
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

func routeFiresOn(csv, status string) bool {
	for _, s := range strings.Split(csv, ",") {
		if strings.ToLower(strings.TrimSpace(s)) == status {
			return true
		}
	}
	return false
}

func scanChannel(scan func(...any) error, j *Journal) (NotificationChannel, error) {
	var (
		ch       NotificationChannel
		cfgBytes []byte
		created  sql.NullString
		updated  sql.NullString
	)
	if err := scan(&ch.ID, &ch.TenantID, &ch.Name, &ch.Kind, &cfgBytes, &created, &updated); err != nil {
		return NotificationChannel{}, err
	}
	ch.ConfigJSON = append(json.RawMessage(nil), cfgBytes...)
	if created.Valid {
		if t, perr := j.parseTime(created.String); perr == nil {
			ch.CreatedAt = t
		}
	}
	if updated.Valid {
		if t, perr := j.parseTime(updated.String); perr == nil {
			ch.UpdatedAt = t
		}
	}
	return ch, nil
}

// routeTenants resolves the owning tenants of a workflow and a notification
// channel, refusing when either row is missing. Mirrors grantTenants: a relation
// whose endpoints cannot both be resolved is not a relation worth writing, and a
// missing row was how phantom grants got in through MCP and the CLI.
func (j *Journal) routeTenants(ctx context.Context, workflowID, channelID string) (string, string, error) {
	var wfTenant, chTenant sql.NullString
	err := j.db.QueryRowContext(ctx,
		j.bind(`SELECT tenant_id FROM workflows WHERE id = $1`), workflowID).Scan(&wfTenant)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("journal: notification route: unknown workflow %q", workflowID)
	}
	if err != nil {
		return "", "", fmt.Errorf("journal: notification route: lookup workflow tenant: %w", err)
	}
	err = j.db.QueryRowContext(ctx,
		j.bind(`SELECT tenant_id FROM notification_channels WHERE id = $1`), channelID).Scan(&chTenant)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("journal: notification route: unknown channel %q", channelID)
	}
	if err != nil {
		return "", "", fmt.Errorf("journal: notification route: lookup channel tenant: %w", err)
	}
	return wfTenant.String, chTenant.String, nil
}
