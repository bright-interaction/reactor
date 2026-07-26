package server

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/reactor/internal/catalog"
	"github.com/bright-interaction/reactor/internal/credentials"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
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
// credentialTargetAdd appends a rotation delivery target.
//
// Rotation's whole point is that consumers get the new value without a restart,
// and rotation_targets was reachable ONLY through hand-written SQL: no repo
// setter, no caller, no UI, no CLI flag. Every credential an operator could
// create therefore had zero targets, so a rotated value was stored and delivered
// nowhere while the audit trail recorded success.
func (s *Server) credentialTargetAdd(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	c, err := s.Credentials.Get(r.Context(), id)
	if err != nil {
		s.errorPage(w, "get credential", err)
		return
	}
	grace := 0
	if v := strings.TrimSpace(r.PostFormValue("grace_seconds")); v != "" {
		n, cerr := strconv.Atoi(v)
		if cerr != nil || n < 0 {
			s.redirectWithTargetError(w, r, id, "grace seconds must be a non-negative whole number")
			return
		}
		grace = n
	}
	t := credentials.Target{
		Kind:         strings.TrimSpace(r.PostFormValue("kind")),
		URL:          strings.TrimSpace(r.PostFormValue("url")),
		KeyName:      strings.TrimSpace(r.PostFormValue("key_name")),
		SecretID:     strings.TrimSpace(r.PostFormValue("secret_id")),
		GraceSeconds: grace,
	}
	// Validate here rather than at rotation time, which would surface hours
	// later inside a scheduler tick nobody is watching.
	if verr := t.Validate(); verr != nil {
		s.redirectWithTargetError(w, r, id, verr.Error())
		return
	}
	if serr := s.Credentials.SetRotationTargets(r.Context(), id, append(c.RotationTargets, t)); serr != nil {
		s.redirectWithTargetError(w, r, id, serr.Error())
		return
	}
	_ = s.Credentials.AppendAudit(r.Context(), credentials.AuditEntry{
		CredentialID: id,
		Action:       "target.add",
		ActorKind:    "operator",
	})
	s.redirectWithTargetError(w, r, id, "")
}

// credentialTargetDelete removes one target by its position in the list.
func (s *Server) credentialTargetDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	idx, err := strconv.Atoi(chi.URLParam(r, "index"))
	if id == "" || err != nil || idx < 0 {
		http.Error(w, "missing id or index", http.StatusBadRequest)
		return
	}
	c, gerr := s.Credentials.Get(r.Context(), id)
	if gerr != nil {
		s.errorPage(w, "get credential", gerr)
		return
	}
	if idx >= len(c.RotationTargets) {
		s.redirectWithTargetError(w, r, id, "that target no longer exists; the list may have changed since the page was loaded")
		return
	}
	remaining := append(append([]credentials.Target{}, c.RotationTargets[:idx]...), c.RotationTargets[idx+1:]...)
	if serr := s.Credentials.SetRotationTargets(r.Context(), id, remaining); serr != nil {
		s.redirectWithTargetError(w, r, id, serr.Error())
		return
	}
	_ = s.Credentials.AppendAudit(r.Context(), credentials.AuditEntry{
		CredentialID: id,
		Action:       "target.remove",
		ActorKind:    "operator",
	})
	s.redirectWithTargetError(w, r, id, "")
}

// redirectWithTargetError carries a message back to the detail page through the
// flash store. A bare 303 with no message is how "Rotate now" managed to look
// identical whether it worked or did nothing.
func (s *Server) redirectWithTargetError(w http.ResponseWriter, r *http.Request, id, msg string) {
	if msg != "" && s.flash != nil {
		if err := s.flash.put(w, r, map[string]string{"rotate_outcome": msg}); err != nil {
			s.Log.Warn("target: flash put failed", "err", err, "credential_id", id)
		}
	}
	http.Redirect(w, r, "/credentials/"+id, http.StatusSeeOther)
}

// rotationGuard refuses the two provider/auto-rotate combinations that silently
// do the wrong thing, returning the operator-facing reason or "" when the
// combination is sound. Kept separate from the handler so the rule is testable
// without an HTTP request or a wired repo.
//
// The first is destruction, not a mismatch: shared-secret used to be the FIRST
// <option> and therefore the default, and it MINTS a new random value locally.
// An operator adding a real Stripe key with the select untouched, then clicking
// "Rotate now", had the sk_live_ key overwritten with random hex while the audit
// trail recorded rotate.success and Stripe still held the old key. Payments
// break silently and the original is unrecoverable.
func (s *Server) rotationGuard(provider string, autoRotate, ackReplace bool) string {
	if s.Rotator == nil {
		return ""
	}
	canAuto, mintsLocally, err := s.Rotator.ProviderCapabilities(provider)
	switch {
	case err != nil:
		return "unknown provider " + provider
	case mintsLocally && !ackReplace:
		return "provider \"" + provider + "\" REPLACES the stored value with a newly generated random secret on every rotation. That is correct only for a secret Reactor itself issues (an HMAC signing key where Reactor controls both ends). For a key issued by another service it would overwrite the real key with a value that service has never seen, and the integration would break with no warning. Choose \"manual\" for an externally issued key, or tick the acknowledgement to proceed."
	case autoRotate && !canAuto:
		return "provider \"" + provider + "\" cannot rotate automatically, so Auto-rotate would only ever record a reminder and the value would never change. Untick Auto-rotate, or pick a provider that rolls the credential at its source."
	}
	return ""
}

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

	// Guard the two combinations that silently do the wrong thing.
	//
	// The default used to be shared-secret purely because it was the first
	// <option>, and it MINTS a new random value locally. An operator adding a
	// Stripe key with the select untouched, then clicking "Rotate now", had the
	// real sk_live_ key overwritten with random hex while the audit trail
	// recorded rotate.success and Stripe still held the old key. Payments break
	// silently, and there is no way back because the original is gone.
	if reason := s.rotationGuard(provider, autoRotate, r.PostFormValue("confirm_replace") == "on"); reason != "" {
		s.renderCredentialNewError(w, reason, name, service)
		return
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
	// Resolve the provider BEFORE rotating so the outcome can be reported. A
	// reminder-only provider (manual, which service-presets.js selects for every
	// catalog API-key service) makes RotateOne write an audit row and return
	// nil, and this handler used to 303 straight back with NO message at all.
	// So the product's headline button did nothing observable on its most common
	// configuration, and an operator could not tell that from a real rotation.
	var outcome string
	if c, cerr := s.Credentials.Get(r.Context(), id); cerr == nil {
		if canAuto, _, perr := s.Rotator.ProviderCapabilities(c.Provider); perr == nil && !canAuto {
			outcome = "Recorded a rotation reminder. Provider \"" + c.Provider +
				"\" cannot roll this credential programmatically, so the stored value is unchanged: rotate it at the issuing service, then paste the new value into \"Update value\" below."
		}
	}
	if err := s.Rotator.RotateOne(r.Context(), id); err != nil {
		s.errorPage(w, "rotate", err)
		return
	}
	if outcome == "" {
		outcome = "Rotation ran. Check the audit trail below for the delivery result of each target; a credential with no rotation targets is stored but delivered nowhere."
	}
	if err := s.flash.put(w, r, map[string]string{"rotate_outcome": outcome}); err != nil {
		s.Log.Warn("rotate: flash put failed", "err", err, "credential_id", id)
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
      <option value="manual">manual -- Reactor never changes the value; rotating records a reminder only. Correct for every externally issued API key (Stripe, Slack, SendGrid, ...)</option>
      <option value="cloudflare">cloudflare -- rolls the token AT Cloudflare and stores the new one</option>
      <option value="aws-iam">aws-iam -- rolls the access key pair AT AWS and stores the new one</option>
      <option value="shared-secret">shared-secret -- REPLACES the value with a new random secret. Only for a secret Reactor itself issues, e.g. an HMAC signing key</option>
    </select>
  </label>
  <label>Value <input type="password" name="value" required autocomplete="new-password" placeholder="initial plaintext, encrypted on write"></label>
  <label class="inline"><input type="checkbox" name="confirm_replace"> I understand <code>shared-secret</code> replaces the stored value with a newly generated random secret (only needed for that provider)</label>
  <label class="inline"><input type="checkbox" name="auto_rotate"> Auto-rotate</label>
  <label>Rotation interval (days, 0 = no schedule) <input type="number" name="interval_days" min="0" value="0"></label>
  <button type="submit">Create</button>
  <a href="/credentials" class="btn-secondary">Cancel</a>
</form>
<script src="/assets/service-presets.js"></script>
`)
	return b.String()
}
