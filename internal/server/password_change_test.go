package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/reactor/internal/auth"
)

// TestUsersSetPasswordIsAdminOnly: issuing a password for ANOTHER user is a
// full account takeover primitive, so it must be admin-only even though it
// lives on the same page as the member-visible user list.
func TestUsersSetPasswordIsAdminOnly(t *testing.T) {
	t.Parallel()
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	req := httptest.NewRequest(http.MethodPost, "/users/usr_1/password", strings.NewReader("password=some-long-password"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withUser(req.Context(), auth.User{ID: "m", Role: auth.RoleMember, TenantID: "default"}))
	req = withChiParam(req, "id", "usr_1")
	rec := httptest.NewRecorder()
	s.usersSetPassword(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a MEMBER got %d from POST /users/{id}/password, want 403; setting another user's password is account takeover", rec.Code)
	}
}

// TestAccountRendersPasswordForm keeps the self-service surface reachable
// through the UI. The audit's operator bar is that a non-developer must get
// there without docs-diving or API calls, and a form nobody renders is a
// feature nobody has.
func TestAccountRendersPasswordForm(t *testing.T) {
	t.Parallel()
	body := accountPasswordForm(true, "alice")
	for _, want := range []string{
		`action="/account/password"`,
		`name="current_password"`,
		`name="new_password"`,
		`name="confirm_password"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the account password form is missing %s", want)
		}
	}
	// Env-var BasicAuth mode has no user row to rotate, so no form.
	if got := accountPasswordForm(false, ""); got != "" {
		t.Fatalf("password form rendered with auth unwired: %q", got)
	}
}

// TestAccountChangePasswordRejectsMismatch is the cheap input guard: a typo in
// the confirmation must not silently set the first value.
func TestAccountChangePasswordRejectsMismatch(t *testing.T) {
	t.Parallel()
	s := &Server{Auth: stubPasswordAdmin{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	form := "current_password=old-password&new_password=aaaaaaaaaa&confirm_password=bbbbbbbbbb"
	req := httptest.NewRequest(http.MethodPost, "/account/password", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withUser(req.Context(), auth.User{ID: "u1", Username: "alice", Role: auth.RoleMember}))
	rec := httptest.NewRecorder()
	s.accountChangePassword(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched confirmation returned %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "do not match") {
		t.Fatalf("no mismatch message rendered: %s", rec.Body.String())
	}
}

// stubPasswordAdmin embeds the interface so only the methods a test actually
// reaches need a body; anything else would panic loudly rather than silently
// pass. The mismatch check runs before any store call, so nothing is invoked.
type stubPasswordAdmin struct{ AuthAdmin }

// changeOKAdmin accepts the change and mints a session, so the handler's
// post-change cookie behaviour can be observed.
type changeOKAdmin struct {
	AuthAdmin
	created int
}

func (a *changeOKAdmin) ChangePassword(context.Context, string, string, string) error { return nil }
func (a *changeOKAdmin) CreateSession(context.Context, string, string, string, time.Duration) (string, error) {
	a.created++
	return "fresh-session-value", nil
}

// TestPasswordChangeKeepsThisSession pins the UX half of the rotate.
//
// ChangePassword revokes EVERY session, which is the security-relevant part:
// whoever may know the old password loses their cookies. But signing the
// operator out of their own browser as well adds nothing, and a rotate that
// logs you out is a rotate people avoid doing. So the handler re-mints THIS
// session and the browser stays signed in.
func TestPasswordChangeKeepsThisSession(t *testing.T) {
	t.Parallel()
	adm := &changeOKAdmin{}
	s := &Server{Auth: adm, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	form := "current_password=old-password&new_password=a-new-password&confirm_password=a-new-password"
	req := httptest.NewRequest(http.MethodPost, "/account/password", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withUser(req.Context(), auth.User{ID: "u1", Username: "alice"}))
	rec := httptest.NewRecorder()
	s.accountChangePassword(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if adm.created != 1 {
		t.Fatalf("CreateSession called %d times, want 1; the current session must be re-minted", adm.created)
	}
	var sess *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session cookie was set after the change")
	}
	if sess.MaxAge < 0 || sess.Value == "" {
		t.Fatalf("the session cookie was CLEARED (value=%q maxage=%d); the operator would be signed out of their own browser", sess.Value, sess.MaxAge)
	}
	if !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode {
		t.Fatalf("the re-minted cookie lost its hardening: httponly=%v samesite=%v", sess.HttpOnly, sess.SameSite)
	}
	if loc := rec.Header().Get("Location"); loc == "/login?changed=1" {
		t.Fatal("redirected to /login; the user should land back on their account still signed in")
	}
}
