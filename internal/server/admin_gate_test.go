package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brightinteraction/reactor/internal/auth"
)

// TestRequireAdminMW is the regression test for the RBAC ship-blocker
// where privileged mutations (workflow code, credentials, triggers) were
// reachable by any authenticated member. The route-group gate must 403 a
// member, allow an admin, and pass through entirely when auth is unwired.
func TestRequireAdminMW(t *testing.T) {
	t.Parallel()

	reached := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}

	cases := []struct {
		name     string
		wired    bool
		user     *auth.User
		wantCode int
	}{
		{"auth unwired passes through", false, nil, http.StatusOK},
		{"admin allowed", true, &auth.User{ID: "u1", Role: auth.RoleAdmin}, http.StatusOK},
		{"member forbidden", true, &auth.User{ID: "u2", Role: auth.RoleMember}, http.StatusForbidden},
		{"anonymous unauthorized", true, nil, http.StatusUnauthorized},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{}
			if tc.wired {
				// A non-nil AuthAdmin so s.Auth == nil is false; the
				// gate logic itself never calls into the store.
				s.Auth = &auth.Store{}
			}
			h := s.requireAdminMW(http.HandlerFunc(reached))

			req := httptest.NewRequest(http.MethodPost, "/credentials", nil)
			if tc.user != nil {
				req = req.WithContext(withUser(req.Context(), *tc.user))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}
