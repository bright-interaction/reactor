package server

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/brightinteraction/reactor/internal/catalog"
	"github.com/brightinteraction/reactor/internal/credentials"
	"github.com/brightinteraction/reactor/internal/runtime/journal"
)

// catLabel is the human label for a catalog category in the UI.
func catLabel(cat string) string {
	switch cat {
	case "crm":
		return "CRM"
	case "project-management":
		return "Project management"
	case "cms":
		return "CMS"
	case "payments":
		return "Payments"
	case "email":
		return "Email"
	case "dev":
		return "Developer"
	default:
		return cat
	}
}

// credentialNewForm renders the form an operator fills in to add a new
// credential through the UI. The CLI surface (`reactor vault add`) still
// works; this is for non-dev tenants who never want to open a terminal.
func (s *Server) credentialNewForm(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, page{
		Title:   "New credential",
		Heading: "New credential",
		Body:    template.HTML(credentialNewBody("", "")),
	})
}

// credentialCreate handles POST /credentials. Body is application/x-www-
// form-urlencoded with the same fields as `reactor vault add`. On success
// the row + encrypted vault blob land atomically; the dashboard redirects
// to the detail page.
func (s *Server) credentialCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	service := strings.TrimSpace(r.PostFormValue("service"))
	provider := strings.TrimSpace(r.PostFormValue("provider"))
	value := r.PostFormValue("value")
	autoRotate := r.PostFormValue("auto_rotate") == "on"
	intervalDaysStr := strings.TrimSpace(r.PostFormValue("interval_days"))

	if name == "" || provider == "" || value == "" {
		s.renderCredentialNewError(w, "name, provider, and value are required", name, service)
		return
	}
	intervalDays := 0
	if intervalDaysStr != "" {
		n, err := strconv.Atoi(intervalDaysStr)
		if err != nil || n < 0 {
			s.renderCredentialNewError(w, "interval_days must be a non-negative integer", name, service)
			return
		}
		intervalDays = n
	}

	id := name
	if err := s.Credentials.Create(r.Context(), credentials.CreateParams{
		ID:                   id,
		Name:                 name,
		Service:              service,
		Provider:             provider,
		AutoRotate:           autoRotate,
		RotationIntervalDays: intervalDays,
	}); err != nil {
		s.renderCredentialNewError(w, "create credential: "+err.Error(), name, service)
		return
	}
	if err := s.Vault.Put(r.Context(), id, []byte(value)); err != nil {
		s.renderCredentialNewError(w, "vault put: "+err.Error(), name, service)
		return
	}
	_ = s.Credentials.AppendAudit(r.Context(), credentials.AuditEntry{
		CredentialID: id,
		Action:       "create.dashboard",
		ActorKind:    "operator",
	})
	http.Redirect(w, r, "/credentials/"+id, http.StatusSeeOther)
}

func (s *Server) renderCredentialNewError(w http.ResponseWriter, msg, name, service string) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	render(w, layout, page{
		Title:   "New credential",
		Heading: "New credential",
		Body:    template.HTML(credentialNewBody(msg, "")) + template.HTML(`<input type="hidden" id="prefill-name" value="`+template.HTMLEscapeString(name)+`"><input type="hidden" id="prefill-service" value="`+template.HTMLEscapeString(service)+`">`),
	})
}

// credentialRotate handles POST /credentials/{id}/rotate. Drives the
// rotator's per-credential pipeline (mint -> encrypt -> persist ->
// deliver to targets -> audit) and redirects back to the detail page.
// The audit row + last_rotated_at update tell the operator what
// happened on the next render.
func (s *Server) credentialRotate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := s.Rotator.RotateOne(r.Context(), id); err != nil {
		s.errorPage(w, "rotate", err)
		return
	}
	http.Redirect(w, r, "/credentials/"+id, http.StatusSeeOther)
}

// credentialGrant handles POST /credentials/{id}/grants with a
// workflow_id form field. Idempotent (Journal.GrantSecret does
// ON CONFLICT UPDATE) so repeated submits don't error.
func (s *Server) credentialGrant(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	wf := strings.TrimSpace(r.PostFormValue("workflow_id"))
	if wf == "" {
		http.Error(w, "workflow_id required", http.StatusBadRequest)
		return
	}
	// Accept a slug too; resolve to id if the table knows it.
	if wfID, err := s.Journal.WorkflowIDBySlug(r.Context(), wf); err == nil && wfID != "" {
		wf = wfID
	}
	if err := s.Journal.GrantSecret(r.Context(), wf, id, "dashboard", ""); err != nil {
		s.errorPage(w, "grant secret", err)
		return
	}
	_ = s.Credentials.AppendAudit(r.Context(), credentials.AuditEntry{
		CredentialID: id,
		Action:       "grant.dashboard",
		ActorKind:    "operator",
		WorkflowID:   wf,
	})
	http.Redirect(w, r, "/credentials/"+id, http.StatusSeeOther)
}

// credentialRevoke handles POST /credentials/{id}/grants/{workflow_id}/revoke.
// Errors via 404 if the grant didn't exist (idempotent operators get a
// readable page rather than a silent 200).
func (s *Server) credentialRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	wf := chi.URLParam(r, "workflow_id")
	// Mirror credentialGrant: the revoke URL may carry a slug from the
	// rendered detail page (WorkflowSlugByID resolves wf_id -> slug),
	// so resolve back to wf_id before the DELETE.
	if wfID, err := s.Journal.WorkflowIDBySlug(r.Context(), wf); err == nil && wfID != "" {
		wf = wfID
	}
	if err := s.Journal.RevokeSecret(r.Context(), wf, id); err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			http.Error(w, fmt.Sprintf("no grant for workflow %s on credential %s", wf, id), http.StatusNotFound)
			return
		}
		s.errorPage(w, "revoke secret", err)
		return
	}
	_ = s.Credentials.AppendAudit(r.Context(), credentials.AuditEntry{
		CredentialID: id,
		Action:       "revoke.dashboard",
		ActorKind:    "operator",
		WorkflowID:   wf,
	})
	http.Redirect(w, r, "/credentials/"+id, http.StatusSeeOther)
}

// credentialNewBody renders the add-credential form. errMsg is shown
// inline above the fields when non-empty.
func credentialNewBody(errMsg, _ string) string {
	var b strings.Builder
	if errMsg != "" {
		b.WriteString(`<div class="err">` + template.HTMLEscapeString(errMsg) + `</div>`)
	}
	// Catalog quick-add: pick a known service to pre-fill its suggested vault
	// key name + where to get the key. service-presets.js fills the form below.
	b.WriteString(`<h3>Quick-add a service</h3><p class="muted">Pick a service to pre-fill its key name and where to find the key, then paste your key into the form.</p>`)
	b.WriteString(`<div class="svc-presets">`)
	lastCat := ""
	for _, svc := range catalog.APIKeyServices() {
		if svc.Category != lastCat {
			b.WriteString(`<span class="svc-cat">` + template.HTMLEscapeString(catLabel(svc.Category)) + `</span>`)
			lastCat = svc.Category
		}
		hint := svc.KeyHint + " | " + svc.AuthScheme + " | Docs: " + svc.Docs
		fmt.Fprintf(&b, `<button type="button" class="svc-preset" data-key-name="%s" data-service="%s" data-hint="%s">%s</button>`,
			template.HTMLEscapeString(svc.KeyName), template.HTMLEscapeString(svc.ID),
			template.HTMLEscapeString(hint), template.HTMLEscapeString(svc.Name))
	}
	b.WriteString(`</div><p id="svc-preset-note" class="muted oauth-preset-note"></p>`)

	b.WriteString(`
<form method="POST" action="/credentials" class="form" id="credential-form">
  <p class="muted">Adds a credential to the vault. Encrypted on write. The plaintext value is read-only after submission; rotate it later via the "Rotate now" button on the detail page.</p>
  <label>Name <input type="text" name="name" required pattern="[a-z0-9_\-]+" title="lowercase letters, digits, underscore, hyphen" autocomplete="off"></label>
  <label>Service <input type="text" name="service" placeholder="e.g. stripe, sendgrid" autocomplete="off"></label>
  <label>Provider
    <select name="provider" required>
      <option value="shared-secret">shared-secret (random 32-byte HMAC key)</option>
      <option value="cloudflare">cloudflare (rolls token in place)</option>
      <option value="aws-iam">aws-iam (self-rotating IAM access key pair)</option>
      <option value="manual">manual (no auto-rotate, audit reminder only)</option>
    </select>
  </label>
  <label>Value <input type="password" name="value" required autocomplete="new-password" placeholder="initial plaintext, encrypted on write"></label>
  <label class="inline"><input type="checkbox" name="auto_rotate"> Auto-rotate</label>
  <label>Rotation interval (days, 0 = no schedule) <input type="number" name="interval_days" min="0" value="0"></label>
  <button type="submit">Create</button>
  <a href="/credentials" class="btn-secondary">Cancel</a>
</form>
<script src="/assets/service-presets.js"></script>
`)
	return b.String()
}
