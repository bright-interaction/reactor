package main

import (
	"strings"
	"testing"
)

func page(body string, headers map[string]string) Page {
	h := map[string]string{}
	for k, v := range headers {
		h[strings.ToLower(k)] = strings.ToLower(v)
	}
	return Page{BodyLower: strings.ToLower(body), Headers: h}
}

func TestFingerprint(t *testing.T) {
	tests := []struct {
		name    string
		page    Page
		wantCMS string
		minSig  int
	}{
		{"wordpress", page(`<link rel="https://api.w.org/"><script src="/wp-content/x.js">`, map[string]string{"X-Pingback": "https://s/xmlrpc.php"}), "WordPress", 2},
		{"ghost", page(`<meta name="generator" content="Ghost 5.2"><a href="/ghost/">`, nil), "Ghost", 2},
		{"webflow", page(`<html data-wf-page="abc" data-wf-site="def">`, nil), "Webflow", 2},
		{"drupal-header", page(`<html>nothing obvious</html>`, map[string]string{"X-Generator": "Drupal 10 (https://www.drupal.org)"}), "Drupal", 1},
		{"shopify", page(`<script src="https://cdn.shopify.com/s/x.js">`, map[string]string{"X-ShopId": "12345"}), "Shopify", 2},
		{"squarespace", page(`<!-- This is Squarespace. -->`, nil), "Squarespace", 1},
		{"unknown", page(`<html><body>just a custom site</body></html>`, nil), "Unknown / headless", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cms, sig := fingerprint(tt.page)
			if cms != tt.wantCMS {
				t.Fatalf("cms = %q, want %q (signals %v)", cms, tt.wantCMS, sig)
			}
			if len(sig) < tt.minSig {
				t.Fatalf("got %d signals, want >= %d: %v", len(sig), tt.minSig, sig)
			}
		})
	}
}

func TestConfidenceAndHint(t *testing.T) {
	if confidence(nil) != "none" || confidence([]string{"a"}) != "medium" || confidence([]string{"a", "b"}) != "high" {
		t.Fatal("confidence buckets wrong")
	}
	if !strings.Contains(connectorHint("WordPress"), "wordpress-key") {
		t.Fatal("WordPress hint should name the vault key")
	}
	if !strings.Contains(connectorHint("Unknown / headless"), "headless") {
		t.Fatal("unknown hint should mention headless")
	}
}
