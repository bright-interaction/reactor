package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
