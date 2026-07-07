package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/bright-interaction/reactor/internal/auth"
)

// SessionCookieName is the dashboard's session cookie name.
const SessionCookieName = "reactor_sess"

// userCtxKey is the context key the session middleware uses to stash
// the resolved user; per-request handlers read it via UserFromContext.
type userCtxKey struct{}

// UserFromContext returns the user the auth middleware resolved for
// this request. Returns ok=false on routes that bypass auth (healthz,
// webhook, signal) or when the daemon ships without auth wired.
func UserFromContext(ctx context.Context) (auth.User, bool) {
	u, ok := ctx.Value(userCtxKey{}).(auth.User)
	return u, ok
}

// authStore is the surface the middleware needs from the auth store.
// Defined here at the consumer so tests can stub.
type authStore interface {
	Authenticate(ctx context.Context, username, password string) (auth.User, error)
	ResolveSession(ctx context.Context, cookieValue string) (auth.User, error)
	ResolveAPIToken(ctx context.Context, raw string) (auth.User, error)
	CountUsers(ctx context.Context) (int, error)
}

// SessionAuthConfig wires the multi-source auth middleware. The
// middleware is mounted alongside (and BEFORE) the legacy BasicAuth
// middleware in the chain; it short-circuits the legacy gate when it
// successfully resolves a session OR an API token, OR when there is
// at least one user in the users table (in which case the legacy
// env-var BasicAuth no longer applies).
type SessionAuthConfig struct {
	// Store resolves sessions + tokens + falls back to user-table
	// BasicAuth (replacing the env-var BasicAuth) when a request
	// arrives with Authorization: Basic.
	Store authStore

	// LegacyAllowNoAuth mirrors BasicAuthConfig.AllowNoAuth; when no
	// users exist AND no env-var creds are set AND this is true, the
	// middleware passes traffic through. Otherwise 503.
	LegacyAllowNoAuth bool
}

// SessionAuth returns the middleware. Resolution order:
//
//  1. Cookie `reactor_sess` -> session store lookup.
//  2. `Authorization: Bearer <token>` -> api_tokens store lookup.
//  3. `Authorization: Basic <user>:<pw>` -> Authenticate via the users
//     table when at least one user exists, else fall back to the
//     legacy env-var BasicAuth (the layer below this one).
//  4. None of the above + no users + no env creds -> redirect to
//     /login when a browser arrives (Accept: text/html), else 401.
//
// Public routes (healthz, webhook, signal, mcp, login, assets) skip
// the gate.
func SessionAuth(cfg SessionAuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSessionAuthExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if cfg.Store == nil {
				// No store wired (test deployments); defer to the legacy
				// BasicAuth middleware in the chain.
				next.ServeHTTP(w, r)
				return
			}

			ctx := r.Context()

			// (1) cookie -> session.
			if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
				u, err := cfg.Store.ResolveSession(ctx, c.Value)
				if err == nil {
					r = r.WithContext(withUser(ctx, u))
					next.ServeHTTP(w, r)
					return
				}
				// Bad cookie: clear it so the browser does not keep
				// retrying.
				http.SetCookie(w, &http.Cookie{
					Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1,
					HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
			}

			// (2) Bearer token.
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
				if raw != "" {
					if u, err := cfg.Store.ResolveAPIToken(ctx, raw); err == nil {
						r = r.WithContext(withUser(ctx, u))
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			// (3) Basic against the users table.
			if user, pass, ok := r.BasicAuth(); ok && user != "" {
				if u, err := cfg.Store.Authenticate(ctx, user, pass); err == nil {
					r = r.WithContext(withUser(ctx, u))
					next.ServeHTTP(w, r)
					return
				}
				// Falls through to (4); BasicAuth credentials might
				// still match the legacy env-var BasicAuth below.
			}

			// (4) No user resolved + no users in DB + AllowNoAuth ->
			// pass through; else require login.
			if n, err := cfg.Store.CountUsers(ctx); err == nil && n == 0 {
				// No users provisioned yet. The legacy BasicAuth in the
				// chain below handles the env-var path (or 503 / pass).
				next.ServeHTTP(w, r)
				return
			}

			// Users exist + no resolved identity -> redirect or 401.
			if wantsHTML(r) {
				http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
				return
			}
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		})
	}
}

// isSessionAuthExempt mirrors isPublicRoute but also exempts /login
// and /assets so the operator can reach the login form + its CSS
// without already being logged in.
func isSessionAuthExempt(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case path == "/login":
		return true
	case strings.HasPrefix(path, "/webhook/"):
		return true
	case strings.HasPrefix(path, "/signal/"):
		return true
	case strings.HasPrefix(path, "/assets/"):
		return true
	case path == "/docs" || strings.HasPrefix(path, "/docs/"):
		// Docs are public so an operator on /login can still read
		// them while figuring out how to sign in.
		return true
	}
	// /mcp is intentionally NOT exempt: it runs through SessionAuth so an
	// MCP client must present a Bearer API token (per-user identity +
	// RBAC) instead of relying on the legacy shared BasicAuth.
	return false
}

func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html") ||
		strings.Contains(accept, "*/*") ||
		accept == ""
}

func withUser(ctx context.Context, u auth.User) context.Context {
	return context.WithValue(ctx, userCtxKey{}, u)
}
