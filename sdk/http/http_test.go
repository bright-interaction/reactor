package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientGetHappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("missing bearer header: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("User-Agent") != "test/1.0" {
			t.Errorf("user-agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"hello": "world"})
	}))
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), Bearer: "secret-token", UserAgent: "test/1.0"}
	var out map[string]string
	if err := c.Get(context.Background(), srv.URL, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if out["hello"] != "world" {
		t.Fatalf("unexpected body: %v", out)
	}
}

func TestClientRetryOn5xx(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := &Client{
		HTTPClient: srv.Client(),
		Retry:      Retry{Max: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	}
	var out map[string]bool
	if err := c.Get(context.Background(), srv.URL, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3 (two 500s + one 200)", hits.Load())
	}
}

func TestClientNon2xxReturnsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{HTTPClient: srv.Client(), Retry: Retry{Max: 1}}
	err := c.Get(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatalf("want error")
	}
	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type = %T, want *Error", err)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", apiErr.Status)
	}
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&Error{Status: 500}, true},
		{&Error{Status: 502}, true},
		{&Error{Status: 408}, true},
		{&Error{Status: 429}, true},
		{&Error{Status: 400}, false},
		{&Error{Status: 404}, false},
		{context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		if got := IsRetryable(tc.err); got != tc.want {
			t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestClientStaticHeaders(t *testing.T) {
	var gotAuth, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVer = r.Header.Get("Notion-Version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	// Headers cover non-Bearer auth (a raw token) + a version pin, and a
	// Headers Authorization overrides the Bearer default.
	c := &Client{Bearer: "should-be-overridden", Headers: map[string]string{
		"Authorization":  "raw-token-123",
		"Notion-Version": "2022-06-28",
	}}
	if err := c.Get(context.Background(), srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "raw-token-123" {
		t.Fatalf("Authorization = %q, want raw-token-123 (Headers should win over Bearer)", gotAuth)
	}
	if gotVer != "2022-06-28" {
		t.Fatalf("Notion-Version = %q", gotVer)
	}
}
