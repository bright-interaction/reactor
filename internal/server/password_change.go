package server

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/reactor/internal/auth"
)

// accountChangePassword is the self-service rotate: any signed-in user changes
// their OWN password, and must supply the current one to do it. Without that
// check a stolen session cookie could take the account over outright by
// rotating the password out from under its owner.
//
// On success every session for the user is destroyed (including this one), so
// the browser lands on /login. That is the intended behaviour: a rotate usually
// happens because the old password may be known to someone else, and leaving
// other cookies alive would keep their access open.
func (s *Server) accountChangePassword(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		http.Error(w, "user accounts are not configured on this deployment", http.StatusServiceUnavailable)
		return
	}
	u, ok := UserFromContext(r.Context())
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")
	if next != confirm {
		s.renderAccountPasswordError(w, r, "The new password and its confirmation do not match.")
		return
	}

	switch err := s.Auth.ChangePassword(r.Context(), u.ID, current, next); {
	case errors.Is(err, auth.ErrInvalidPassword):
		// Deliberately not "wrong current password" vs "no such user": this is
		// an authenticated surface, so the only useful distinction is whether
		// the current password matched.
		s.renderAccountPasswordError(w, r, "That is not your current password.")
		return
	case errors.Is(err, auth.ErrPasswordTooShort):
		s.renderAccountPasswordError(w, r, "The new password must be at least 8 characters.")
		return
	case err != nil:
		s.errorPage(w, "change password", err)
		return
	}

	// Sessions are gone; clear the cookie so the browser stops sending it.
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login?changed=1", http.StatusSeeOther)
}

func (s *Server) renderAccountPasswordError(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	s.renderPage(w, r, page{
		Title:   "Account",
		Heading: "Change password",
		Body: template.HTML(`<p class="err">` + template.HTMLEscapeString(msg) +
			`</p><p><a href="/account">Back to your account</a></p>`),
	})
}

// usersSetPassword is the admin reset: an administrator issues a new password
// for another user without knowing the old one. This is the recovery path the
// login page has always pointed at ("ask an admin to mint a fresh one for you
// via the Users page") and which that page could not actually perform, so the
// only real recovery was deleting and recreating the account, losing its MFA
// factors, API tokens and audit history.
func (s *Server) usersSetPassword(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if s.Auth == nil {
		http.Error(w, "user accounts are not configured on this deployment", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	next := r.PostFormValue("password")

	switch err := s.Auth.SetPassword(r.Context(), id, next); {
	case errors.Is(err, auth.ErrPasswordTooShort):
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	case errors.Is(err, auth.ErrNotFound):
		http.Error(w, "user not found", http.StatusNotFound)
		return
	case err != nil:
		s.errorPage(w, "set password", err)
		return
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

// accountPasswordForm renders the self-service change-password card for the
// account page. Returns empty when auth is not wired (env-var BasicAuth only),
// since there is no user row to rotate in that mode.
func accountPasswordForm(authWired bool, username string) string {
	if !authWired {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<h2>Password</h2>`)
	fmt.Fprintf(&b, `<p class="muted">Signed in as <code>%s</code>. Changing your password signs you out of every session, including this one.</p>`,
		template.HTMLEscapeString(username))
	b.WriteString(`<form method="POST" action="/account/password" class="form">
  <label>Current password <input type="password" name="current_password" required autocomplete="current-password"></label>
  <label>New password (min 8 characters) <input type="password" name="new_password" required minlength="8" autocomplete="new-password"></label>
  <label>Confirm new password <input type="password" name="confirm_password" required minlength="8" autocomplete="new-password"></label>
  <button type="submit" class="btn-primary">Change password</button>
</form>`)
	return b.String()
}
