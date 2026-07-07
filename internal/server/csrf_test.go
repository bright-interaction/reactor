package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCSRFAllowsSafeMethods(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(CSRF(silentHandler))
	defer srv.Close()
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req, _ := http.NewRequest(m, srv.URL+"/credentials", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", m, resp.StatusCode)
		}
	}
}

func TestCSRFRejectsCrossOriginPost(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(CSRF(silentHandler))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/credentials", strings.NewReader(""))
	req.Header.Set("Origin", "https://attacker.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestCSRFAcceptsSameOriginPost(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(CSRF(silentHandler))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/credentials", strings.NewReader(""))
	req.Header.Set("Origin", srv.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCSRFRejectsMissingOriginAndReferer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(CSRF(silentHandler))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/credentials", strings.NewReader(""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestCSRFExemptsWebhookSignalMCP(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(CSRF(silentHandler))
	defer srv.Close()
	for _, path := range []string{"/webhook/whk_x", "/signal/sig_x", "/mcp", "/mcp/v1"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(""))
		req.Header.Set("Origin", "https://attacker.example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (exempt path must skip CSRF check)", path, resp.StatusCode)
		}
	}
}

func TestCSRFAcceptsRefererFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(CSRF(silentHandler))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/credentials", strings.NewReader(""))
	req.Header.Set("Referer", srv.URL+"/some/page?with=query")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Referer same-origin should pass)", resp.StatusCode)
	}
}
