package server

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/brightinteraction/reactor/internal/catalog"
	"github.com/brightinteraction/reactor/internal/oauth"
)

// integrations is the Make-style single page to connect apps: OAuth services
// get a one-click Connect (consent popup), key-based services get a one-time
// "Add key", and connected accounts show their status. It is catalog-driven, so
// new services appear here automatically.
func (s *Server) integrations(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		s.renderPage(w, r, page{Title: "Integrations", Heading: "Integrations",
			Body: template.HTML(`<p class="muted">OAuth connections are not wired on this deployment. API-key services can still be added on the <a href="/credentials/new">credentials</a> page.</p>`)})
		return
	}
	ctx := r.Context()
	tenant := viewerTenant(r)
	conns, _ := s.OAuth.ListConnections(ctx, tenant)
	providers, _ := s.OAuth.ListProviders(ctx)
	isAdmin := false
	if u, ok := UserFromContext(ctx); ok {
		isAdmin = u.IsAdmin()
	}
	s.renderPage(w, r, page{
		Title:   "Integrations",
		Heading: "Integrations",
		Body:    template.HTML(integrationsBody(conns, providers, isAdmin, r.URL.Query().Get("ok"), r.URL.Query().Get("err"))),
	})
}

func integrationsBody(conns []oauth.Connection, providers []oauth.Provider, isAdmin bool, ok, errMsg string) string {
	var b strings.Builder
	if errMsg != "" {
		b.WriteString(`<div class="callout">` + template.HTMLEscapeString(errMsg) + `</div>`)
	}
	if ok != "" {
		b.WriteString(`<div class="callout" style="border-color:var(--ok);color:var(--ok)">Connected ` + template.HTMLEscapeString(ok) + `.</div>`)
	}

	// Which providers an admin has configured (client id + enabled) and which
	// the viewer has already connected.
	configured := map[string]bool{}
	for _, p := range providers {
		if p.ClientID != "" && p.Enabled {
			configured[p.ProviderID] = true
		}
	}
	connected := map[string]bool{}
	for _, c := range conns {
		connected[c.ProviderID] = true
	}

	b.WriteString(`<p class="muted">Connect the apps your workflows use. OAuth services open a consent window (click, accept, done); key-based services take an API key once. Tokens are encrypted at rest and never shown.</p>`)

	// Connected accounts.
	b.WriteString(`<h2>Connected accounts</h2>`)
	if len(conns) == 0 {
		b.WriteString(`<p class="empty">Nothing connected yet. Pick an app below.</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>App</th><th>Name</th><th>Status</th><th></th></tr></thead><tbody>`)
		for _, c := range conns {
			name := c.ProviderID
			if svc, ok := catalog.ByID(c.ProviderID); ok {
				name = svc.Name
			}
			tag := `<span class="tag tag-on">connected</span>`
			if c.Status != "connected" {
				tag = `<span class="tag tag-off">` + template.HTMLEscapeString(c.Status) + `</span>`
			}
			fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td><form method="POST" action="/connections/%s/delete" class="form-inline" onsubmit="return confirm('Disconnect %s?');"><button type="submit" class="btn-link">disconnect</button></form></td></tr>`,
				template.HTMLEscapeString(name), template.HTMLEscapeString(c.Name), tag,
				urlEsc(c.ID), template.JSEscapeString(c.Name))
		}
		b.WriteString(`</tbody></table>`)
	}

	// Catalog grid, grouped by category.
	b.WriteString(`<h2>Add an integration</h2>`)
	lastCat := ""
	for _, svc := range catalog.All() {
		if svc.Category != lastCat {
			if lastCat != "" {
				b.WriteString(`</div>`)
			}
			b.WriteString(`<h3>` + template.HTMLEscapeString(catLabel(svc.Category)) + `</h3><div class="intg-grid">`)
			lastCat = svc.Category
		}
		b.WriteString(integrationCard(svc, connected[svc.ID], configured[svc.ID], isAdmin))
	}
	if lastCat != "" {
		b.WriteString(`</div>`)
	}
	b.WriteString(`<script src="/assets/integrations.js"></script>`)
	return b.String()
}

// integrationCard renders one service tile with the right call to action.
func integrationCard(svc catalog.Service, connected, configured, isAdmin bool) string {
	var action string
	switch {
	case connected:
		action = `<span class="tag tag-on">Connected</span> <a href="/connections" class="btn-link">manage</a>`
	case svc.HasOAuth() && configured:
		// One-click Connect: a same-origin POST submitted into a popup window.
		action = fmt.Sprintf(`<form method="POST" action="/connections/%s/start" target="reactor-oauth" class="intg-connect" data-popup>`+
			`<input type="hidden" name="name" value="%s">`+
			`<button type="submit" class="btn-primary">Connect</button></form>`,
			urlEsc(svc.ID), template.HTMLEscapeString(svc.Name))
	case svc.Auth == catalog.APIKey:
		action = `<a href="/credentials/new" class="btn-secondary">Add key</a>`
	case isAdmin:
		// OAuth-only service whose app isn't configured yet.
		action = `<a href="/oauth-providers" class="btn-secondary">Set up OAuth</a>`
	default:
		action = `<span class="muted">ask an admin to enable</span>`
	}

	method := "API key"
	if svc.HasOAuth() {
		method = "OAuth"
		if svc.Auth == catalog.APIKey {
			method = "OAuth or API key"
		}
	}
	return fmt.Sprintf(`<div class="intg-card"><div class="intg-head"><span class="intg-name">%s</span><span class="intg-method">%s</span></div><div class="intg-action">%s</div></div>`,
		template.HTMLEscapeString(svc.Name), template.HTMLEscapeString(method), action)
}
