package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/brightinteraction/reactor/internal/auth"
	"github.com/brightinteraction/reactor/internal/credentials"
	"github.com/brightinteraction/reactor/internal/migrate"
	"github.com/brightinteraction/reactor/internal/registry"
	"github.com/brightinteraction/reactor/internal/runtime/journal"
	_ "modernc.org/sqlite"
)

// newAuthServer builds a httptest.Server with a real auth store + a
// seeded admin user. Returns the URL, the journal, and the auth store
// so tests can mint sessions / tokens directly when needed.
func newAuthServer(t *testing.T, allowNoAuth bool) (string, *journal.Journal, *auth.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "s.db")
	url := "sqlite://" + dbPath
	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := migrate.Up(context.Background(), silent, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	j := journal.New(db, journal.EngineSQLite)
	cred := credentials.New(db, credentials.EngineSQLite)
	reg := registry.New(filepath.Join(dir, "workflows"))
	store := auth.New(db, auth.EngineSQLite)
	if _, err := store.CreateUser(context.Background(), "alice", "secret-pass", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	s := &Server{
		Journal:     j,
		Credentials: cred,
		Registry:    reg,
		Auth:        store,
		Log:         silent,
		Version:     "test",
		BasicAuth:   BasicAuthConfig{AllowNoAuth: allowNoAuth},
	}
	r := chi.NewRouter()
	s.Mount(r)
	srv := httptest.NewServer(r)
	cleanup := func() { srv.Close(); db.Close() }
	return srv.URL, j, store, cleanup
}

func TestUnauthRequestRedirectsToLogin(t *testing.T) {
	t.Parallel()
	url, _, _, cleanup := newAuthServer(t, false)
	defer cleanup()

	cl := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequest(http.MethodGet, url+"/", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location = %q, want /login...", loc)
	}
}

func TestLoginPostMintsSession(t *testing.T) {
	t.Parallel()
	url, _, _, cleanup := newAuthServer(t, false)
	defer cleanup()

	cl := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	form := makeForm("username", "alice", "password", "secret-pass", "next", "/users")
	resp := mustPostFormAuth(t, cl, url+"/login", form)
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 303\n%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Location") != "/users" {
		t.Fatalf("Location = %q", resp.Header.Get("Location"))
	}
	cookie := pickCookie(resp.Cookies(), SessionCookieName)
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("session cookie missing: %+v", resp.Cookies())
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie missing security attrs: %+v", cookie)
	}

	// Authenticated GET succeeds.
	req, _ := http.NewRequest(http.MethodGet, url+"/", nil)
	req.AddCookie(cookie)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("post-login GET status = %d", resp2.StatusCode)
	}
}

func TestBearerTokenAuthenticates(t *testing.T) {
	t.Parallel()
	url, _, store, cleanup := newAuthServer(t, false)
	defer cleanup()

	users, _ := store.ListUsers(context.Background())
	if len(users) == 0 {
		t.Fatal("no seeded user")
	}
	raw, _, err := store.MintAPIToken(context.Background(), users[0].ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodGet, url+"/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer GET status = %d", resp.StatusCode)
	}
}

func TestRBACMemberBlockedFromWorkflowDelete(t *testing.T) {
	t.Parallel()
	url, j, store, cleanup := newAuthServer(t, false)
	defer cleanup()

	ctx := context.Background()
	// Create a member account and a workflow.
	if _, err := store.CreateUser(ctx, "bob", "secret-pass", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateWorkflow(ctx, "wf_x", "x-flow", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Mint a session for bob via direct store call so the test
	// bypasses the login form.
	cookie, _ := store.CreateSession(ctx, lookupUserID(t, store, "bob"), "ua", "1.2.3.4", 1<<30)

	cl := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/workflows/x-flow/delete", strings.NewReader(""))
	req.Header.Set("Origin", url)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 403\n%s", resp.StatusCode, body)
	}
}

func TestRBACAdminCanDeleteWorkflow(t *testing.T) {
	t.Parallel()
	url, j, store, cleanup := newAuthServer(t, false)
	defer cleanup()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_x", "x-flow", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Move the seeded run for wf_1 from newTestJournal's fixture isn't
	// applicable here because newAuthServer doesn't seed it; the new
	// workflow has no runs so delete should proceed.

	cookie, _ := store.CreateSession(ctx, lookupUserID(t, store, "alice"), "", "", 1<<30)
	cl := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/workflows/x-flow/delete", strings.NewReader(""))
	req.Header.Set("Origin", url)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 303\n%s", resp.StatusCode, body)
	}
}

func TestLogoutClearsSessionCookie(t *testing.T) {
	t.Parallel()
	url, _, store, cleanup := newAuthServer(t, false)
	defer cleanup()
	cookie, _ := store.CreateSession(context.Background(), lookupUserID(t, store, "alice"), "", "", 1<<30)

	cl := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/logout", strings.NewReader(""))
	req.Header.Set("Origin", url)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", resp.StatusCode)
	}
	cleared := pickCookie(resp.Cookies(), SessionCookieName)
	if cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("logout did not clear cookie; got %+v", cleared)
	}
	// The session row should be gone (cookie value no longer resolves).
	if _, err := store.ResolveSession(context.Background(), cookie); err == nil {
		t.Fatal("session row still resolves after logout")
	}
}

func makeForm(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func mustPostFormAuth(t *testing.T, cl *http.Client, target string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if u, err := url.Parse(target); err == nil {
		req.Header.Set("Origin", u.Scheme+"://"+u.Host)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func pickCookie(list []*http.Cookie, name string) *http.Cookie {
	for _, c := range list {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func lookupUserID(t *testing.T, store *auth.Store, username string) string {
	t.Helper()
	users, _ := store.ListUsers(context.Background())
	for _, u := range users {
		if u.Username == username {
			return u.ID
		}
	}
	t.Fatalf("user %q not seeded", username)
	return ""
}
