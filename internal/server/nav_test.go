package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brightinteraction/reactor/internal/auth"
)

func navForUser(u *auth.User) string {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if u != nil {
		req = req.WithContext(withUser(req.Context(), *u))
	}
	return string(navFor(req))
}

func TestNavRoleScoping(t *testing.T) {
	t.Parallel()
	adminOnly := []string{"/postmortems", "/notifications", "/audit", "/users", "/tenants", "/plans"}
	common := []string{"/", "/runs", "/errors", "/account", "/docs"}

	member := navForUser(&auth.User{ID: "m", Role: auth.RoleMember})
	for _, href := range adminOnly {
		if strings.Contains(member, `href="`+href+`"`) {
			t.Errorf("member nav should NOT contain admin link %s", href)
		}
	}
	for _, href := range common {
		if !strings.Contains(member, `href="`+href+`"`) {
			t.Errorf("member nav should contain %s", href)
		}
	}

	admin := navForUser(&auth.User{ID: "a", Role: auth.RoleAdmin})
	for _, href := range append(append([]string{}, adminOnly...), common...) {
		if !strings.Contains(admin, `href="`+href+`"`) {
			t.Errorf("admin nav should contain %s", href)
		}
	}

	// No user (no-auth / pre-login) defaults to the full operator menu.
	none := navForUser(nil)
	for _, href := range adminOnly {
		if !strings.Contains(none, `href="`+href+`"`) {
			t.Errorf("no-auth nav should contain %s (full operator menu)", href)
		}
	}
}
