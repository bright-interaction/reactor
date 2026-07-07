package server

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// workflowTemplate is a starter automation: a curated brief the AI codegen
// turns into a working workflow. Templates give a non-developer a one-click
// starting point instead of a blank page.
type workflowTemplate struct {
	ID          string
	Name        string
	Category    string
	Description string
	Brief       string
}

// templateCatalog is the built-in starter set. The briefs are detailed enough
// for the generator to produce a runnable workflow.
var templateCatalog = []workflowTemplate{
	{
		ID: "webhook-to-slack", Name: "Webhook to Slack alert", Category: "Notify",
		Description: "Receive a webhook and post a formatted message to a Slack channel.",
		Brief:       "A webhook-triggered workflow that takes the incoming JSON payload and posts a concise, formatted summary message to a Slack channel via an incoming webhook URL stored as a credential. Include the key fields from the payload in the message.",
	},
	{
		ID: "daily-summary-email", Name: "Daily summary email", Category: "Report",
		Description: "On a schedule, gather data and email a summary.",
		Brief:       "A cron-triggered workflow that runs every morning at 8am, fetches a JSON report from an HTTP API endpoint, formats the results into a short readable summary, and sends it as an email via SMTP credentials. Use an idempotency key per day so a retry never sends twice.",
	},
	{
		ID: "form-to-crm", Name: "Form submission to CRM", Category: "Sales",
		Description: "Capture a form webhook, enrich the lead, and upsert it to a CRM.",
		Brief:       "A webhook-triggered workflow that receives a contact form submission (name, email, company), enriches it with an HTTP call to an enrichment API, scores the lead, and upserts the contact into a CRM via its REST API. Each external call is its own step with an idempotency key on the email.",
	},
	{
		ID: "url-health-check", Name: "Scheduled health check", Category: "Monitor",
		Description: "Poll a URL on a schedule and alert when it is down.",
		Brief:       "A cron-triggered workflow that runs every 5 minutes, does an HTTP GET to a configured URL, and if the response is not 2xx or takes longer than 3 seconds, sends an alert to a notification channel. Otherwise it records success and exits quietly.",
	},
	{
		ID: "stripe-receipt", Name: "Stripe payment to notification", Category: "Payments",
		Description: "On a Stripe payment webhook, notify the team.",
		Brief:       "A Stripe-webhook-triggered workflow (provider stripe) that fires on a successful payment event, extracts the amount, currency, and customer email, and posts a celebratory notification to a Slack channel. Verify it only acts on payment success events.",
	},
	{
		ID: "rss-to-webhook", Name: "RSS feed to webhook", Category: "Integrate",
		Description: "Poll an RSS feed and forward new items to a webhook.",
		Brief:       "A cron-triggered workflow that runs hourly, fetches an RSS/Atom feed over HTTP, parses the items, and for each item newer than the last run posts the title and link to an outbound webhook URL. Use an idempotency key per item GUID so an item is forwarded exactly once.",
	},
}

// templates renders the starter-template gallery. Browsable by anyone; the
// "Use template" action runs the AI generator (admin-gated like /generate).
func (s *Server) templates(w http.ResponseWriter, r *http.Request) {
	render := func(body string) {
		s.renderPage(w, r, page{Title: "Templates", Heading: "Templates", Body: template.HTML(body)})
	}
	render(templatesBody(s.Generator != nil))
}

func templatesBody(generatorEnabled bool) string {
	var b strings.Builder
	b.WriteString(`<p class="muted">Start from a proven automation instead of a blank page. "Use template" feeds the brief to the AI builder, which generates + registers a working workflow you can then edit.</p>`)
	if !generatorEnabled {
		b.WriteString(`<div class="callout">The AI builder is not configured on this deployment (no Anthropic API key), so one-click build is off. You can still copy a brief below and build via the CLI or paste it into the New workflow form.</div>`)
	}
	b.WriteString(`<div class="tpl-grid">`)
	for _, t := range templateCatalog {
		b.WriteString(`<div class="tpl-card">`)
		fmt.Fprintf(&b, `<div class="tpl-cat">%s</div>`, template.HTMLEscapeString(t.Category))
		fmt.Fprintf(&b, `<h3 class="tpl-name">%s</h3>`, template.HTMLEscapeString(t.Name))
		fmt.Fprintf(&b, `<p class="tpl-desc">%s</p>`, template.HTMLEscapeString(t.Description))
		if generatorEnabled {
			fmt.Fprintf(&b, `<form method="POST" action="/generate" class="tpl-use"><input type="hidden" name="brief" value="%s"><button type="submit" class="btn-primary">Use template</button></form>`,
				template.HTMLEscapeString(t.Brief))
		}
		fmt.Fprintf(&b, `<details class="tpl-brief"><summary>view brief</summary><p class="muted">%s</p></details>`, template.HTMLEscapeString(t.Brief))
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}
