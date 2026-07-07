package server

import (
	"strings"
	"testing"
)

func TestOAuthProvidersBodyRendersPresets(t *testing.T) {
	html := oauthProvidersBody(nil, "https://app.example/oauth/callback", "")
	for _, want := range []string{
		`class="oauth-presets"`,
		`data-provider-id="google"`,
		`data-provider-id="microsoft"`,
		`data-provider-id="github"`,
		`https://login.microsoftonline.com/common/oauth2/v2.0/authorize`,
		`offline_access`,
		`id="oauth-provider-form"`,
		`/assets/oauth-presets.js`,
		`https://app.example/oauth/callback`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered providers page missing %q", want)
		}
	}
}
