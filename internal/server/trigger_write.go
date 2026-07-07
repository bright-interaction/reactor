package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	robfig "github.com/robfig/cron/v3"

	"github.com/bright-interaction/reactor/internal/credentials"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// workflowCreateTrigger handles POST /workflows/{slug}/triggers. Kind
// is read from the form body. Webhook triggers auto-generate a public
// token id + a 32-byte HMAC secret that lands in the vault under a
// dedicated credential row so the rotation engine can roll it later.
// Cron triggers take a spec + optional timezone and insert directly.
func (s *Server) workflowCreateTrigger(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromRequest(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	kind := strings.TrimSpace(r.PostFormValue("kind"))
	if kind == "" {
		kind = "webhook"
	}

	ctx := r.Context()
	wfID, err := s.Journal.WorkflowIDBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			http.Error(w, "workflow not registered", http.StatusNotFound)
			return
		}
		s.errorPage(w, "lookup workflow", err)
		return
	}

	switch kind {
	case "webhook":
		s.createWebhookTrigger(w, r, slug, wfID)
	case "cron":
		s.createCronTrigger(w, r, slug, wfID)
	case "chain":
		s.createChainTrigger(w, r, slug, wfID)
	default:
		http.Error(w, "unsupported kind "+kind, http.StatusBadRequest)
	}
}

// createChainTrigger handles POST /workflows/{slug}/triggers with
// kind=chain. The slug here is the DOWNSTREAM workflow; source_slug
// is the upstream workflow whose terminal status drives this one.
func (s *Server) createChainTrigger(w http.ResponseWriter, r *http.Request, slug, downstreamID string) {
	sourceSlug := strings.TrimSpace(r.PostFormValue("source_slug"))
	onStatuses := strings.TrimSpace(r.PostFormValue("on_statuses"))
	if onStatuses == "" {
		onStatuses = "succeeded"
	}
	if sourceSlug == "" {
		http.Error(w, "source_slug is required", http.StatusBadRequest)
		return
	}
	sourceID, err := s.Journal.WorkflowIDBySlug(r.Context(), sourceSlug)
	if err != nil {
		http.Error(w, "source workflow not found: "+sourceSlug, http.StatusNotFound)
		return
	}
	if _, err := s.Journal.CreateChainTrigger(r.Context(), downstreamID, sourceID, onStatuses); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/workflows/"+slug, http.StatusSeeOther)
}

func (s *Server) createWebhookTrigger(w http.ResponseWriter, r *http.Request, slug, wfID string) {
	if s.Vault == nil || s.Credentials == nil {
		http.Error(w, "webhook triggers need a vault wired; start the daemon with vault credentials configured", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	provider := strings.TrimSpace(r.PostFormValue("provider"))
	if provider == "" {
		provider = "generic"
	}
	tokenID, err := journal.NewTokenID()
	if err != nil {
		s.errorPage(w, "generate token", err)
		return
	}

	// Mint a fresh 32-byte HMAC shared secret + park it in the vault
	// under a dedicated credential row. The credential id mirrors the
	// trigger's intent so an operator scanning /credentials sees what
	// it's for without opening the trigger row.
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		s.errorPage(w, "generate secret", err)
		return
	}
	secretHex := hex.EncodeToString(secretBytes)
	credID := "webhook_" + slug + "_" + tokenID[:8]
	if err := s.Credentials.Create(ctx, credentials.CreateParams{
		ID:       credID,
		Name:     credID,
		Service:  "reactor-webhook",
		Provider: "shared-secret",
	}); err != nil {
		s.errorPage(w, "create secret credential", err)
		return
	}
	if err := s.Vault.Put(ctx, credID, []byte(secretHex)); err != nil {
		s.errorPage(w, "vault put secret", err)
		return
	}

	cfgMap := map[string]any{"created_by": "dashboard"}
	if r.PostFormValue("sync") == "on" {
		// Synchronous: the webhook waits for the run + returns its output.
		cfgMap["sync"] = true
		if ts, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("sync_timeout"))); ts > 0 {
			cfgMap["timeout_seconds"] = ts
		}
	}
	cfg, _ := json.Marshal(cfgMap)
	if _, err := s.Journal.CreateWebhookTrigger(ctx, wfID, tokenID, credID, provider, cfg); err != nil {
		s.errorPage(w, "create webhook trigger", err)
		return
	}
	// Stash the freshly-minted secret in a server-side single-use
	// store keyed by a random cookie. The redirect-target page reads
	// it once + deletes. Putting the secret in the URL would land in
	// access logs, browser history, and any Referer header sent from
	// sub-resource loads on the redirect target. The encrypted vault
	// row is the only durable copy; this is the last readable
	// rendering.
	if err := s.flash.put(w, r, map[string]string{
		"new_webhook_token":  tokenID,
		"new_webhook_secret": secretHex,
	}); err != nil {
		s.errorPage(w, "flash put", err)
		return
	}
	http.Redirect(w, r, "/workflows/"+slug, http.StatusSeeOther)
}

func (s *Server) createCronTrigger(w http.ResponseWriter, r *http.Request, slug, wfID string) {
	spec := strings.TrimSpace(r.PostFormValue("spec"))
	timezone := strings.TrimSpace(r.PostFormValue("timezone"))
	if spec == "" {
		http.Error(w, "cron spec required (5-field cron expression)", http.StatusBadRequest)
		return
	}
	// Validate the spec + timezone the SAME way the cron driver parses them, so
	// an invalid expression is rejected here with actionable feedback instead of
	// being silently accepted and then never firing.
	full := spec
	if timezone != "" {
		full = "CRON_TZ=" + timezone + " " + spec
	}
	if _, err := robfig.ParseStandard(full); err != nil {
		http.Error(w, "invalid cron spec or timezone: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfgMap := map[string]string{"spec": spec}
	if timezone != "" {
		cfgMap["timezone"] = timezone
	}
	cfg, _ := json.Marshal(cfgMap)
	if _, err := s.Journal.CreateCronTrigger(r.Context(), wfID, cfg); err != nil {
		s.errorPage(w, "create cron trigger", err)
		return
	}
	http.Redirect(w, r, "/workflows/"+slug, http.StatusSeeOther)
}

// renderTriggersSection draws the Triggers panel on the workflow detail
// page: a one-time secret flash from a fresh webhook creation, a table
// of existing triggers with per-row delete forms, then two stacked
// add-trigger forms (webhook + cron) so the operator never has to type
// SQL or use the CLI to start receiving traffic.
func renderTriggersSection(d workflowDetailData) string {
	var b strings.Builder
	b.WriteString(`<h2>Triggers</h2>`)

	if d.NewWebhookToken != "" && d.NewWebhookSecret != "" {
		b.WriteString(`<div class="callout">`)
		b.WriteString(`<p><strong>Webhook trigger created.</strong> Copy the curl line below now. The HMAC secret is encrypted in the vault and will not be shown again on subsequent loads (rotate it via the credentials page if you lose it).</p>`)
		fmt.Fprintf(&b, `<pre>SECRET=%s
TOKEN=%s
BODY='{"hello":"world"}'
SIG=$(printf '%%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')
curl -X POST http://127.0.0.1:7777/webhook/$TOKEN \
  -H "Content-Type: application/json" \
  -H "X-Webhook-Signature: sha256=$SIG" \
  -H "X-Webhook-Delivery: $(uuidgen)" \
  -d "$BODY"</pre>`,
			template.HTMLEscapeString(d.NewWebhookSecret),
			template.HTMLEscapeString(d.NewWebhookToken),
		)
		b.WriteString(`</div>`)
	}

	if len(d.Triggers) == 0 {
		b.WriteString(`<p class="muted">No triggers yet. Add a webhook or cron trigger below to start receiving runs.</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>Kind</th><th>State</th><th>Detail</th><th>Last fired</th><th>Error</th><th></th></tr></thead><tbody>`)
		for _, t := range d.Triggers {
			detail := triggerDetailCell(t)
			lastFired := "-"
			if t.LastFiredAt != nil {
				lastFired = t.LastFiredAt.UTC().Format("2006-01-02 15:04:05Z")
			}
			errCell := ""
			if t.LastError != "" {
				errCell = `<span class="err">` + template.HTMLEscapeString(truncate(t.LastError, 80)) + `</span>`
			}
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td class="muted">%s</td><td>%s</td><td>`,
				template.HTMLEscapeString(string(t.Kind)),
				template.HTMLEscapeString(t.State),
				detail,
				template.HTMLEscapeString(lastFired),
				errCell,
			)
			if d.TriggerWritesEnabled {
				slugEsc := template.URLQueryEscaper(d.Slug)
				idEsc := template.URLQueryEscaper(t.ID)
				if t.State == "active" {
					fmt.Fprintf(&b, `<form method="POST" action="/workflows/%s/triggers/%s/pause" class="form-inline"><button type="submit" class="btn-link">pause</button></form>`, slugEsc, idEsc)
				} else {
					fmt.Fprintf(&b, `<form method="POST" action="/workflows/%s/triggers/%s/resume" class="form-inline"><button type="submit" class="btn-link">resume</button></form>`, slugEsc, idEsc)
				}
				fmt.Fprintf(&b, `<form method="POST" action="/workflows/%s/triggers/%s/delete" class="form-inline"><button type="submit" class="btn-link">delete</button></form>`,
					slugEsc, idEsc,
				)
				if t.Kind == journal.TriggerCron {
					var cfg struct {
						Spec     string `json:"spec"`
						Timezone string `json:"timezone"`
					}
					_ = json.Unmarshal(t.Config, &cfg)
					fmt.Fprintf(&b, `<details><summary class="muted">edit</summary>
<form method="POST" action="/workflows/%s/triggers/%s/edit" class="form-inline">
  <input type="text" name="spec" value="%s" required>
  <input type="text" name="timezone" value="%s" placeholder="IANA tz">
  <button type="submit">save</button>
</form></details>`, slugEsc, idEsc,
						template.HTMLEscapeString(cfg.Spec),
						template.HTMLEscapeString(cfg.Timezone))
				}
			}
			b.WriteString(`</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}

	if !d.TriggerWritesEnabled {
		b.WriteString(`<p class="muted">Trigger writes disabled (server started without a vault). Use <code>reactor</code> CLI to manage triggers.</p>`)
		return b.String()
	}

	b.WriteString(renderAddTriggerSection(template.URLQueryEscaper(d.Slug)))
	return b.String()
}

// renderAddTriggerSection draws the "Add a trigger" block: a segmented picker
// (Webhook / Schedule / Chain) that reveals only the selected form via pure CSS
// radio state, so the operator is never staring at three forms at once. The
// Schedule panel carries the visual cron builder (cron-builder.js writes the
// computed expression into the spec field); raw cron stays under "Advanced".
func renderAddTriggerSection(sl string) string {
	var b strings.Builder
	b.WriteString(`<h3>Add a trigger</h3><div class="trig-add">`)
	// Radios first so the CSS general-sibling selectors can reveal panels.
	b.WriteString(`<input type="radio" name="trigtype" id="trig-webhook" checked>` +
		`<input type="radio" name="trigtype" id="trig-cron">` +
		`<input type="radio" name="trigtype" id="trig-chain">`)
	b.WriteString(`<div class="trig-seg" role="tablist">` +
		`<label for="trig-webhook">Webhook</label>` +
		`<label for="trig-cron">Schedule</label>` +
		`<label for="trig-chain">After another workflow</label></div>`)
	b.WriteString(`<div class="trig-panels">`)

	// Webhook panel.
	fmt.Fprintf(&b, `<div class="trig-panel" id="panel-webhook"><form method="POST" action="/workflows/%s/triggers" class="form">
  <input type="hidden" name="kind" value="webhook">
  <label>Provider <select name="provider"><option value="generic">generic (X-Webhook-Signature sha256=hex)</option><option value="github">github (X-Hub-Signature-256)</option><option value="stripe">stripe (Stripe-Signature t=...,v1=...)</option></select></label>
  <label class="form-inline"><input type="checkbox" name="sync"> Synchronous: wait for the run and return its result to the caller (build a request/response API)</label>
  <label>Sync timeout <input type="number" name="sync_timeout" min="1" max="120" value="30"> seconds (max 120)</label>
  <p class="muted">Generates a public token + a 32-byte HMAC secret. Both land in the vault as a new credential the rotation engine can roll later. Synchronous responses return JSON {run_id, status, output} where output is the last step's data.</p>
  <button type="submit" class="btn-primary">Create webhook trigger</button>
</form></div>`, sl)

	// Schedule panel = visual cron builder + advanced raw cron.
	fmt.Fprintf(&b, `<div class="trig-panel" id="panel-cron"><form method="POST" action="/workflows/%s/triggers" class="form" data-cron-form>
  <input type="hidden" name="kind" value="cron">
  %s
  <label>Timezone (optional) <input type="text" name="timezone" placeholder="Europe/Stockholm" autocomplete="off"></label>
  <button type="submit" class="btn-primary">Create schedule</button>
</form></div>`, sl, cronBuilderMarkup())

	// Chain panel.
	fmt.Fprintf(&b, `<div class="trig-panel" id="panel-chain"><form method="POST" action="/workflows/%s/triggers" class="form">
  <input type="hidden" name="kind" value="chain">
  <label>Source workflow slug <input type="text" name="source_slug" required pattern="[a-z][a-z0-9-]*" placeholder="upstream-workflow-slug" autocomplete="off"></label>
  <label>Fire on (comma-separated) <input type="text" name="on_statuses" value="succeeded" placeholder="succeeded[,failed,failed_dlq]"></label>
  <p class="muted">When the upstream workflow's run lands in any of these terminal statuses, this workflow gets dispatched with a payload carrying <code>source_run_id</code>, <code>source_status</code>, <code>source_workflow_slug</code>, and (for failures) <code>source_error_text</code>.</p>
  <button type="submit" class="btn-primary">Create chain trigger</button>
</form></div>`, sl)

	b.WriteString(`</div></div>`) // .trig-panels, .trig-add
	b.WriteString(`<script src="/assets/cron-builder.js"></script>`)
	return b.String()
}

// cronBuilderMarkup is the non-dev schedule builder. All builder controls are
// nameless so they never POST; cron-builder.js writes the computed 5-field
// expression into the named spec input (under Advanced). data-cron attributes
// are the JS hooks.
func cronBuilderMarkup() string {
	dows := ""
	for i, d := range []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"} {
		// cron day-of-week: Sun=0..Sat=6; Mon=1..Sun=7 also accepted, but use 0-6.
		val := i + 1
		if val == 7 {
			val = 0
		}
		dows += fmt.Sprintf(`<label class="cron-dow"><input type="checkbox" data-dow="%d">%s</label>`, val, d)
	}
	months := ""
	for i, m := range []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"} {
		months += fmt.Sprintf(`<option value="%d">%s</option>`, i+1, m)
	}
	return `<div class="cron-build" data-cron-build>
  <div class="cron-row">
    <label>Run
      <select data-cron="freq">
        <option value="minutes">every N minutes</option>
        <option value="hourly">every N hours</option>
        <option value="daily" selected>every day</option>
        <option value="weekly">every week</option>
        <option value="monthly">every month</option>
        <option value="yearly">every year</option>
      </select>
    </label>
    <label class="cron-field" data-when="minutes,hourly">every
      <input type="number" data-cron="interval" min="1" max="59" value="5" style="width:64px"> <span data-cron="unit">minutes</span>
    </label>
    <label class="cron-field" data-when="daily,weekly,monthly,yearly">at
      <input type="time" data-cron="time" value="09:00">
    </label>
    <label class="cron-field" data-when="monthly,yearly">on day
      <input type="number" data-cron="dom" min="1" max="31" value="1" style="width:64px">
    </label>
    <label class="cron-field" data-when="yearly">of
      <select data-cron="month">` + months + `</select>
    </label>
  </div>
  <div class="cron-field" data-when="weekly"><div class="cron-dows" data-cron="dows">` + dows + `</div></div>
  <div class="cron-summary" data-cron="summary"></div>
  <details class="cron-advanced"><summary>Advanced: edit raw cron</summary>
    <label>Cron spec (5-field) <input type="text" name="spec" data-cron="spec" required placeholder="0 9 * * *" autocomplete="off"></label>
  </details>
</div>`
}

// triggerDetailCell renders the per-row "Detail" column: token id for
// webhooks (the secret is intentionally not shown), spec + timezone for
// cron, source slug + statuses for chain, generic kind label otherwise.
func triggerDetailCell(t journal.Trigger) string {
	switch t.Kind {
	case journal.TriggerWebhook:
		return `<code>POST /webhook/` + template.HTMLEscapeString(t.TokenID) + `</code>`
	case journal.TriggerCron:
		var cfg struct {
			Spec     string `json:"spec"`
			Timezone string `json:"timezone"`
		}
		_ = json.Unmarshal(t.Config, &cfg)
		out := `<code>` + template.HTMLEscapeString(cfg.Spec) + `</code>`
		if cfg.Timezone != "" {
			out += ` <span class="muted">` + template.HTMLEscapeString(cfg.Timezone) + `</span>`
		}
		if next := nextCronFire(cfg.Spec, cfg.Timezone); !next.IsZero() {
			out += ` <span class="muted">next: ` + template.HTMLEscapeString(next.UTC().Format("2006-01-02 15:04Z")) + `</span>`
		}
		return out
	case journal.TriggerWorkflowComplete:
		var cfg struct {
			SourceWorkflowID string `json:"source_workflow_id"`
			OnStatuses       string `json:"on_statuses"`
		}
		_ = json.Unmarshal(t.Config, &cfg)
		return `<span class="muted">after</span> <code>` +
			template.HTMLEscapeString(cfg.SourceWorkflowID) +
			`</code> <span class="muted">on</span> <code>` +
			template.HTMLEscapeString(cfg.OnStatuses) + `</code>`
	default:
		return `<span class="muted">-</span>`
	}
}

// nextCronFire parses the cron spec + timezone and returns the next
// fire time after now in the configured location. Mirrors the cron
// driver's parser (5-field standard cron with optional CRON_TZ prefix)
// so the dashboard's "next: <ts>" hint matches what the runtime will
// actually do. Returns zero on parse error so the cell renders without
// a "next:" suffix rather than displaying nonsense.
func nextCronFire(spec, timezone string) time.Time {
	if spec == "" {
		return time.Time{}
	}
	full := spec
	if timezone != "" {
		full = "CRON_TZ=" + timezone + " " + spec
	}
	sched, err := robfig.ParseStandard(full)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(time.Now())
}

// workflowDeleteTrigger handles POST /workflows/{slug}/triggers/{trigger_id}/delete.
// Returns 404 if no row matched so a double-click doesn't silently 200.
func (s *Server) workflowDeleteTrigger(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromRequest(w, r)
	if !ok {
		return
	}
	triggerID := chi.URLParam(r, "trigger_id")
	if err := s.Journal.DeleteTrigger(r.Context(), triggerID); err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			http.Error(w, "trigger not found", http.StatusNotFound)
			return
		}
		s.errorPage(w, "delete trigger", err)
		return
	}
	http.Redirect(w, r, "/workflows/"+slug, http.StatusSeeOther)
}
