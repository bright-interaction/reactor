package catalog

import (
	"net/url"
	"strings"
	"testing"
)

func TestCatalogWellFormed(t *testing.T) {
	seen := map[string]bool{}
	keyNames := map[string]bool{}
	for _, s := range All() {
		if s.ID == "" || s.Name == "" || s.Category == "" {
			t.Fatalf("service missing id/name/category: %+v", s)
		}
		if seen[s.ID] {
			t.Fatalf("duplicate service id %q", s.ID)
		}
		seen[s.ID] = true
		if s.AuthScheme == "" || s.Docs == "" || len(s.Ops) == 0 {
			t.Fatalf("service %q missing auth/docs/ops", s.ID)
		}
		switch s.Auth {
		case OAuth:
			for _, raw := range []string{s.AuthURL, s.TokenURL} {
				u, err := url.Parse(raw)
				if err != nil || u.Scheme != "https" || u.Host == "" {
					t.Fatalf("oauth service %q has bad URL %q", s.ID, raw)
				}
			}
		case APIKey:
			if s.KeyName == "" {
				t.Fatalf("api-key service %q missing KeyName", s.ID)
			}
			if keyNames[s.KeyName] {
				t.Fatalf("duplicate KeyName %q", s.KeyName)
			}
			keyNames[s.KeyName] = true
		default:
			t.Fatalf("service %q has unknown auth %q", s.ID, s.Auth)
		}
		// Any service that advertises an OAuth path must have valid https
		// authorize + token URLs (even API-key-primary dual-auth services).
		if s.HasOAuth() {
			for _, raw := range []string{s.AuthURL, s.TokenURL} {
				u, err := url.Parse(raw)
				if err != nil || u.Scheme != "https" || u.Host == "" {
					t.Fatalf("oauth-capable service %q has bad URL %q", s.ID, raw)
				}
			}
		}
	}
}

func TestCatalogHasCRMAndPM(t *testing.T) {
	if len(ByCategory("crm")) < 8 {
		t.Fatalf("want an extensive CRM list, got %d", len(ByCategory("crm")))
	}
	if len(ByCategory("project-management")) < 8 {
		t.Fatalf("want an extensive PM list, got %d", len(ByCategory("project-management")))
	}
	if len(ByCategory("cms")) < 8 {
		t.Fatalf("want an extensive CMS list, got %d", len(ByCategory("cms")))
	}
	for _, id := range []string{"hubspot", "salesforce", "pipedrive", "asana", "jira", "notion", "linear", "wordpress", "webflow", "contentful"} {
		if _, ok := ByID(id); !ok {
			t.Fatalf("expected catalog to include %q", id)
		}
	}
}

func TestOAuthAndAPIKeySplit(t *testing.T) {
	if len(OAuthServices()) < 3 {
		t.Fatalf("want >=3 oauth services, got %d", len(OAuthServices()))
	}
	if len(APIKeyServices()) < 10 {
		t.Fatalf("want >=10 api-key services, got %d", len(APIKeyServices()))
	}
	// Microsoft keeps offline_access (needed for a refresh token).
	ms, _ := ByID("microsoft")
	if !strings.Contains(ms.Scopes, "offline_access") {
		t.Fatalf("microsoft must keep offline_access scope")
	}
	// Dual-auth services support Connect (OAuth) while staying in the API-key
	// list. HubSpot is the canonical example.
	hs, _ := ByID("hubspot")
	if !hs.HasOAuth() {
		t.Fatalf("hubspot should be OAuth-capable")
	}
	var hsInOAuth, hsInKey bool
	for _, s := range OAuthServices() {
		if s.ID == "hubspot" {
			hsInOAuth = true
		}
	}
	for _, s := range APIKeyServices() {
		if s.ID == "hubspot" {
			hsInKey = true
		}
	}
	if !hsInOAuth || !hsInKey {
		t.Fatalf("hubspot should appear in both OAuth (%v) and API-key (%v) lists", hsInOAuth, hsInKey)
	}
}

func TestRenderForAI(t *testing.T) {
	out := RenderForAI()
	for _, want := range []string{"### CRM", "### Project management", "HubSpot", "api.hubapi.com", "vault.MustGet", "oauth:<connection-id>", "docs:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("RenderForAI missing %q", want)
		}
	}
}
