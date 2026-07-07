package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// Notifier is the surface the dashboard uses to fire a test alert.
// Wired by the daemon; nil in read-only deployments.
type Notifier interface {
	TestChannel(ctx context.Context, channelID string) error
}

// notifications renders the channels list + the add-channel form.
func (s *Server) notifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channels, err := s.Journal.ListNotificationChannels(ctx)
	if err != nil {
		s.errorPage(w, "list notification channels", err)
		return
	}
	s.renderPage(w, r, page{
		Title:   "Notifications",
		Heading: "Notifications",
		Body:    template.HTML(notificationsBody(channels, "")),
	})
}

// notificationsCreate handles POST /notifications.
func (s *Server) notificationsCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	kind := strings.TrimSpace(r.PostFormValue("kind"))
	if name == "" || kind == "" {
		s.notificationsError(w, r, "name and kind are required")
		return
	}

	cfg, err := parseChannelConfig(kind, r)
	if err != nil {
		s.notificationsError(w, r, err.Error())
		return
	}
	if _, err := s.Journal.CreateNotificationChannel(r.Context(), name, kind, cfg); err != nil {
		s.notificationsError(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

// notificationsDelete drops the channel after rejecting if any
// workflow routes still reference it.
func (s *Server) notificationsDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Journal.DeleteNotificationChannel(r.Context(), id); err != nil {
		if errors.Is(err, journal.ErrChannelInUse) {
			http.Error(w, err.Error()+"; detach the per-workflow routes first", http.StatusConflict)
			return
		}
		s.errorPage(w, "delete notification channel", err)
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

// notificationsTest fires a synthetic message through the configured
// sender so the operator can confirm credentials + connectivity from
// the dashboard.
func (s *Server) notificationsTest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.Notifier == nil {
		http.Error(w, "notifier not wired on this deployment", http.StatusServiceUnavailable)
		return
	}
	if err := s.Notifier.TestChannel(r.Context(), id); err != nil {
		http.Error(w, "send failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

// workflowNotificationRouteCreate handles POST /workflows/{slug}/notifications.
// Form fields: channel_id, on_statuses (comma-separated; defaults to
// the journal's "failed,failed_dlq" baseline).
func (s *Server) workflowNotificationRouteCreate(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromRequest(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	channelID := strings.TrimSpace(r.PostFormValue("channel_id"))
	if channelID == "" {
		http.Error(w, "channel_id is required", http.StatusBadRequest)
		return
	}
	onStatuses := strings.TrimSpace(r.PostFormValue("on_statuses"))
	if onStatuses == "" {
		onStatuses = "failed,failed_dlq"
	}
	wfID, err := s.Journal.WorkflowIDBySlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			http.Error(w, "workflow not registered", http.StatusNotFound)
			return
		}
		s.errorPage(w, "lookup workflow", err)
		return
	}
	if err := s.Journal.AddNotificationRoute(r.Context(), wfID, channelID, onStatuses); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/workflows/"+slug, http.StatusSeeOther)
}

// workflowNotificationRouteDelete handles POST /workflows/{slug}/notifications/{channel_id}/delete.
func (s *Server) workflowNotificationRouteDelete(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromRequest(w, r)
	if !ok {
		return
	}
	channelID := chi.URLParam(r, "channel_id")
	wfID, err := s.Journal.WorkflowIDBySlug(r.Context(), slug)
	if err != nil {
		s.errorPage(w, "lookup workflow", err)
		return
	}
	if err := s.Journal.DeleteNotificationRoute(r.Context(), wfID, channelID); err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			http.Error(w, "route not found", http.StatusNotFound)
			return
		}
		s.errorPage(w, "delete notification route", err)
		return
	}
	http.Redirect(w, r, "/workflows/"+slug, http.StatusSeeOther)
}

// notificationsError renders the notifications page with an inline
// error pill above the form so the operator does not lose context.
func (s *Server) notificationsError(w http.ResponseWriter, r *http.Request, msg string) {
	channels, _ := s.Journal.ListNotificationChannels(r.Context())
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.renderPage(w, r, page{
		Title:   "Notifications",
		Heading: "Notifications",
		Body:    template.HTML(notificationsBody(channels, msg)),
	})
}

// parseChannelConfig validates + serialises the per-kind form fields.
func parseChannelConfig(kind string, r *http.Request) (json.RawMessage, error) {
	switch kind {
	case journal.ChannelKindSlackWebhook:
		url := strings.TrimSpace(r.PostFormValue("slack_url"))
		if !strings.HasPrefix(url, "https://hooks.slack.com/") {
			return nil, errors.New("slack webhook URL must start with https://hooks.slack.com/")
		}
		return json.Marshal(map[string]any{"url": url})
	case journal.ChannelKindGenericWebhook:
		url := strings.TrimSpace(r.PostFormValue("webhook_url"))
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
			return nil, errors.New("webhook URL must be http(s)")
		}
		headerName := strings.TrimSpace(r.PostFormValue("webhook_header_name"))
		headerValue := strings.TrimSpace(r.PostFormValue("webhook_header_value"))
		headerCredID := strings.TrimSpace(r.PostFormValue("webhook_header_credential_id"))
		cfg := map[string]any{"url": url}
		switch {
		case headerName != "" && headerCredID != "":
			// Reference a vault credential for the header value; the secret
			// is resolved at send time and never stored in config_json.
			cfg["header_name"] = headerName
			cfg["header_credential_id"] = headerCredID
		case headerName != "" && headerValue != "":
			cfg["headers"] = map[string]string{headerName: headerValue}
		}
		return json.Marshal(cfg)
	case journal.ChannelKindEmailSMTP:
		host := strings.TrimSpace(r.PostFormValue("smtp_host"))
		port := strings.TrimSpace(r.PostFormValue("smtp_port"))
		from := strings.TrimSpace(r.PostFormValue("smtp_from"))
		to := strings.TrimSpace(r.PostFormValue("smtp_to"))
		user := strings.TrimSpace(r.PostFormValue("smtp_username"))
		pass := r.PostFormValue("smtp_password")
		passCredID := strings.TrimSpace(r.PostFormValue("smtp_password_credential_id"))
		if host == "" || from == "" || to == "" {
			return nil, errors.New("smtp host, from, and to are required")
		}
		portInt := 587
		if port != "" {
			if _, err := fmt.Sscanf(port, "%d", &portInt); err != nil {
				return nil, errors.New("smtp port must be an integer")
			}
		}
		cfg := map[string]any{
			"host":     host,
			"port":     portInt,
			"from":     from,
			"to":       to,
			"username": user,
		}
		// Prefer a vault credential reference; fall back to a plaintext
		// password only when no credential id is given (legacy path).
		if passCredID != "" {
			cfg["password_credential_id"] = passCredID
		} else {
			cfg["password"] = pass
		}
		return json.Marshal(cfg)
	}
	return nil, fmt.Errorf("unknown channel kind %q", kind)
}

// notificationsBody renders the page contents.
func notificationsBody(channels []journal.NotificationChannel, errMsg string) string {
	var b strings.Builder
	if errMsg != "" {
		b.WriteString(`<div class="err" style="background:#fff;border:1px solid var(--err);padding:12px 16px;margin:0 0 16px;border-radius:3px;">` +
			template.HTMLEscapeString(errMsg) + `</div>`)
	}
	b.WriteString(`<h2>Channels</h2>`)
	if len(channels) == 0 {
		b.WriteString(`<p class="empty">No channels configured. Use the form below to add one, then attach it to a workflow on the workflow detail page.</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>Name</th><th>Kind</th><th>Created</th><th></th></tr></thead><tbody>`)
		for _, c := range channels {
			fmt.Fprintf(&b, `<tr><td>%s</td><td><code>%s</code></td><td class="muted">%s</td><td>
<form method="POST" action="/notifications/%s/test" class="form-inline"><button type="submit">Send test</button></form>
<form method="POST" action="/notifications/%s/delete" class="form-inline" onsubmit="return confirm('Delete channel %s? Active per-workflow routes block the delete.');"><button type="submit" class="btn-link">delete</button></form>
</td></tr>`,
				template.HTMLEscapeString(c.Name),
				template.HTMLEscapeString(c.Kind),
				formatTime(c.CreatedAt),
				template.URLQueryEscaper(c.ID),
				template.URLQueryEscaper(c.ID),
				// onsubmit JS string literal: JSEscapeString, not
				// HTMLEscapeString (an &#39; would decode back to ' and break out).
				template.JSEscapeString(c.Name),
			)
		}
		b.WriteString(`</tbody></table>`)
	}

	b.WriteString(`<h2>Add channel</h2>`)
	b.WriteString(`<form method="POST" action="/notifications" class="form">
  <label>Name <input type="text" name="name" required autocomplete="off" placeholder="e.g. ops-slack, on-call-email"></label>
  <label>Kind
    <select name="kind" required onchange="document.querySelectorAll('.kind-fields').forEach(e=>e.hidden=true);var k=this.value;if(k){var el=document.getElementById('fields-'+k);if(el)el.hidden=false;}">
      <option value="">choose...</option>
      <option value="slack_webhook">Slack (incoming webhook)</option>
      <option value="generic_webhook">Generic JSON webhook</option>
      <option value="email_smtp">Email (SMTP)</option>
    </select>
  </label>
  <fieldset id="fields-slack_webhook" class="kind-fields" hidden>
    <legend>Slack</legend>
    <label>Webhook URL <input type="url" name="slack_url" placeholder="https://hooks.slack.com/services/..."></label>
  </fieldset>
  <fieldset id="fields-generic_webhook" class="kind-fields" hidden>
    <legend>Generic webhook</legend>
    <label>Webhook URL <input type="url" name="webhook_url" placeholder="https://example.com/reactor"></label>
    <label>Optional auth header name <input type="text" name="webhook_header_name" placeholder="X-Auth-Token"></label>
    <label>Auth header value <input type="text" name="webhook_header_value" placeholder="plaintext (stored in config)"></label>
    <label>...or vault credential id <input type="text" name="webhook_header_credential_id" placeholder="cred_... (recommended; secret stays in the vault)"></label>
  </fieldset>
  <fieldset id="fields-email_smtp" class="kind-fields" hidden>
    <legend>Email (SMTP)</legend>
    <label>SMTP host <input type="text" name="smtp_host" placeholder="smtp.gmail.com"></label>
    <label>SMTP port <input type="number" name="smtp_port" value="587" min="1" max="65535"></label>
    <label>From <input type="email" name="smtp_from" placeholder="alerts@example.com"></label>
    <label>To <input type="text" name="smtp_to" placeholder="ops@example.com[, oncall@example.com]"></label>
    <label>Username <input type="text" name="smtp_username" autocomplete="off"></label>
    <label>Password <input type="password" name="smtp_password" autocomplete="new-password" placeholder="plaintext (stored in config)"></label>
    <label>...or vault credential id <input type="text" name="smtp_password_credential_id" placeholder="cred_... (recommended; password stays in the vault)"></label>
  </fieldset>
  <p class="muted">After creating, attach the channel to specific workflows on the workflow detail page. By default, channels fire on <code>failed</code> + <code>failed_dlq</code> terminal status; you can broaden to <code>succeeded</code> per workflow.</p>
  <button type="submit" class="btn-primary">Create channel</button>
</form>`)
	return b.String()
}

// renderNotificationRoutesSection draws the per-workflow notification
// routes on the workflow detail page (called by workflowDetailBody).
func renderNotificationRoutesSection(slug string, routes []journal.NotificationRouteWithChannel, allChannels []journal.NotificationChannel) string {
	var b strings.Builder
	b.WriteString(`<h2>Notifications</h2>`)
	if len(routes) == 0 {
		b.WriteString(`<p class="muted">No channels attached. Pick one below to receive alerts when this workflow's runs terminate.</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>Channel</th><th>Kind</th><th>Fires on</th><th></th></tr></thead><tbody>`)
		for _, r := range routes {
			fmt.Fprintf(&b, `<tr><td>%s</td><td><code>%s</code></td><td><code>%s</code></td><td>
<form method="POST" action="/workflows/%s/notifications/%s/delete" class="form-inline" onsubmit="return confirm('Detach %s from this workflow?');"><button type="submit" class="btn-link">detach</button></form>
</td></tr>`,
				template.HTMLEscapeString(r.ChannelName),
				template.HTMLEscapeString(r.ChannelKind),
				template.HTMLEscapeString(r.OnStatuses),
				template.URLQueryEscaper(slug),
				template.URLQueryEscaper(r.ChannelID),
				// onsubmit JS string literal: JSEscapeString, not HTMLEscapeString.
				template.JSEscapeString(r.ChannelName),
			)
		}
		b.WriteString(`</tbody></table>`)
	}

	// Add form (only when at least one channel exists; otherwise point
	// the operator at /notifications to create one first).
	if len(allChannels) == 0 {
		b.WriteString(`<p class="muted">No channels exist yet. <a href="/notifications">Create a channel</a> first.</p>`)
		return b.String()
	}
	fmt.Fprintf(&b, `<h3>Attach channel</h3>
<form method="POST" action="/workflows/%s/notifications" class="form">
  <label>Channel <select name="channel_id" required>`, template.URLQueryEscaper(slug))
	routed := map[string]bool{}
	for _, r := range routes {
		routed[r.ChannelID] = true
	}
	for _, c := range allChannels {
		if routed[c.ID] {
			continue
		}
		fmt.Fprintf(&b, `<option value="%s">%s (%s)</option>`,
			template.HTMLEscapeString(c.ID),
			template.HTMLEscapeString(c.Name),
			template.HTMLEscapeString(c.Kind),
		)
	}
	b.WriteString(`</select></label>
  <label>Fire on (comma-separated) <input type="text" name="on_statuses" value="failed,failed_dlq" placeholder="failed,failed_dlq[,succeeded]"></label>
  <p class="muted">Defaults to <code>failed,failed_dlq</code>. Add <code>succeeded</code> if you want positive confirmation alerts too.</p>
  <button type="submit" class="btn-primary">Attach</button>
</form>`)
	return b.String()
}
