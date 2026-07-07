package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SecurityHeaders adds the response-header floor every public endpoint
// gets. CSP is intentionally tight (no inline scripts, no third-party
// origins) because the status pages are server-rendered Go html/template
// with one inline <style> block and no script tags. HSTS only fires
// when the request actually arrived over TLS so plain-HTTP demos don't
// pin browsers into broken state.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "0")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// script-src 'self' allows the dashboard's embedded cytoscape +
		// init scripts under /assets/. Inline scripts are still blocked
		// (no 'unsafe-inline'); workflow detail passes DAG data via
		// a <script type="application/json"> island so no
		// inline-execution path is opened up.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"font-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'none'")
		// HSTS only fires when the request actually arrived over TLS, OR
		// when X-Forwarded-Proto: https was set by a trusted proxy
		// (loopback / RFC1918 / RFC4193). Honoring XFP from arbitrary
		// sources lets any attacker pin a domain to HTTPS-only via a
		// plain-HTTP request, which can wedge a misconfigured site for
		// the entire max-age window (here: 2 years).
		if r.TLS != nil || (r.Header.Get("X-Forwarded-Proto") == "https" && isTrustedProxyRequest(r)) {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// BasicAuthConfig configures the BasicAuth middleware.
type BasicAuthConfig struct {
	// User is the username an operator types into the browser prompt.
	User string

	// PasswordSHA256 is the SHA-256 hash of the password (hex-encoded).
	// Hashing avoids holding the plaintext in memory across the
	// middleware's lifetime + makes the env-var presentation safe
	// (operators paste the hash, not the password) once the daemon
	// runs in production.
	//
	// May also carry an argon2id PHC string (starts with $argon2id$);
	// the middleware sniffs the prefix and routes to the right verifier.
	// Argon2id is the recommended default; raw SHA-256 stays supported
	// for backwards compatibility with v0.x setups.
	PasswordSHA256 string

	// AllowNoAuth is the explicit opt-in to run the dashboard with no
	// authentication. When false (default) and User+PasswordSHA256 are
	// empty, BasicAuth refuses every request with 503; operators are
	// expected to either configure credentials or pass this flag (via
	// --insecure-no-auth on `reactor serve`) when they really mean it.
	AllowNoAuth bool

	// Realm shows up in the browser auth prompt.
	Realm string
}

// BasicAuth returns middleware that enforces HTTP Basic auth against
// the configured credentials. Comparison is constant-time on both
// fields so a wrong-username request takes the same time as a wrong-
// password request.
//
// When User + PasswordSHA256 are both empty, BasicAuth is a no-op and
// the middleware passes through. This lets `reactor serve` ship with
// auth opt-in (REACTOR_BASIC_AUTH_USER + REACTOR_BASIC_AUTH_PASSWORD_SHA256 env)
// without breaking the local-demo flow that doesn't set them.
//
// Skips auth on the routes that have their own auth gate:
//
//	POST /webhook/{token}    HMAC verified at the journal layer
//	POST /signal/{token}     128-bit token capability
//	GET  /healthz            liveness probes need to be unauthenticated
func BasicAuth(cfg BasicAuthConfig) func(http.Handler) http.Handler {
	credsMissing := cfg.User == "" || cfg.PasswordSHA256 == ""
	allowNoAuth := credsMissing && cfg.AllowNoAuth
	wantUser := []byte(cfg.User)
	verify := newPasswordVerifier(cfg.PasswordSHA256)
	realm := cfg.Realm
	if realm == "" {
		realm = "reactor"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isPublicRoute(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			// SessionAuth runs first in the chain (F3); if it already
			// resolved a user, skip the legacy BasicAuth gate entirely.
			if _, ok := UserFromContext(r.Context()); ok {
				next.ServeHTTP(w, r)
				return
			}
			if credsMissing {
				if allowNoAuth {
					next.ServeHTTP(w, r)
					return
				}
				// Fail closed. Operators who really want an unauth
				// dashboard set --insecure-no-auth on `reactor serve`,
				// which routes through AllowNoAuth=true above.
				http.Error(w,
					"dashboard is not configured with credentials; set REACTOR_BASIC_AUTH_USER + REACTOR_BASIC_AUTH_PASSWORD_SHA256, or pass --insecure-no-auth to allow unauthenticated access",
					http.StatusServiceUnavailable)
				return
			}
			user, pass, ok := r.BasicAuth()
			userOK := ok && subtle.ConstantTimeCompare([]byte(user), wantUser) == 1
			passOK := ok && verify(pass)
			if !userOK || !passOK {
				w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isPublicRoute(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case path == "/login":
		// Login is by definition reached without a session; exempt so
		// the BasicAuth fail-closed branch does not pre-empt the form.
		return true
	case strings.HasPrefix(path, "/webhook/"):
		return true
	case strings.HasPrefix(path, "/signal/"):
		return true
	case strings.HasPrefix(path, "/assets/"):
		// CSS + cytoscape JS load on the login page too.
		return true
	case path == "/docs" || strings.HasPrefix(path, "/docs/"):
		// Docs are public so an operator on /login can still read
		// them while figuring out how to sign in.
		return true
	}
	return false
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	const hex = "0123456789abcdef"
	out := make([]byte, len(h)*2)
	for i, b := range h {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out)
}

// CSRF returns middleware that rejects state-changing requests whose
// Origin (or Referer, when Origin is absent) does not match the
// dashboard's own origin. Single-origin server-rendered dashboards
// don't need per-form tokens; an Origin gate gives equivalent
// protection against CSRF without threading state through every
// template render.
//
// Modern browsers attach Origin to every cross-site fetch/form POST
// by default, and SameSite=Lax cookie default already blocks third-
// party-initiated POSTs from carrying BasicAuth. The Origin check is
// belt-and-suspenders for older browsers + extension-initiated
// requests + any path where SameSite was not honoured.
//
// Skipped methods: GET, HEAD, OPTIONS (idempotent / read-only).
// Skipped paths: webhook + signal + healthz + mcp (those have their
// own auth + are explicitly cross-origin by design).
//
// The "expected" scheme://host is computed from r.Host + r.TLS so
// the daemon doesn't need to know its public URL ahead of time.
// Trusted-proxy callers may set X-Forwarded-Host and X-Forwarded-Proto
// to override; those are honoured only when isTrustedProxyRequest is
// true, matching the SecurityHeaders HSTS gate.
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStateChangingMethod(r.Method) || isCSRFExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		expected := expectedOrigin(r)
		if expected == "" {
			http.Error(w, "csrf: cannot derive expected origin", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if !equalOrigin(origin, expected) {
				http.Error(w, "csrf: Origin mismatch", http.StatusForbidden)
				return
			}
		} else if ref := r.Header.Get("Referer"); ref != "" {
			if !refererMatchesOrigin(ref, expected) {
				http.Error(w, "csrf: Referer mismatch", http.StatusForbidden)
				return
			}
		} else {
			http.Error(w, "csrf: missing Origin/Referer on state-changing request", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isStateChangingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func isCSRFExempt(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case strings.HasPrefix(path, "/webhook/"):
		return true
	case strings.HasPrefix(path, "/signal/"):
		return true
	case path == "/mcp" || strings.HasPrefix(path, "/mcp/"):
		return true
	}
	return false
}

func expectedOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if isTrustedProxyRequest(r) {
		if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
			scheme = v
		}
		if v := r.Header.Get("X-Forwarded-Host"); v != "" {
			host = v
		}
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func equalOrigin(got, expected string) bool {
	return strings.EqualFold(strings.TrimRight(got, "/"), strings.TrimRight(expected, "/"))
}

func refererMatchesOrigin(referer, expected string) bool {
	// Compare scheme + host only; the path component of Referer is
	// arbitrary on legitimate requests.
	idx := strings.Index(referer, "://")
	if idx == -1 {
		return false
	}
	// Find end of host (next '/' or '?').
	rest := referer[idx+3:]
	end := len(rest)
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		end = i
	}
	got := referer[:idx+3+end]
	return equalOrigin(got, expected)
}

// RateLimit returns middleware that enforces a simple token-bucket
// rate limit per source IP. Public endpoints (webhook + signal) are
// the primary target; status pages are also limited for parity.
//
// burst is the bucket capacity (max instantaneous requests); refill is
// the steady-state requests-per-second once the bucket drains.
//
// Implementation is a tiny in-process map keyed by remote IP, no
// external dep. Replaces by a real limiter (e.g. golang.org/x/time/rate)
// when the daemon needs cross-instance limits in week 11.
func RateLimit(burst int, refill float64) func(http.Handler) http.Handler {
	if burst <= 0 || refill <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	limiter := newIPLimiter(burst, refill)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.allow(clientIP(r)) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limit", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP extracts the source IP. X-Forwarded-For is honoured ONLY
// when the request looks like it came from a trusted proxy (loopback
// + RFC1918 / RFC4193 private ranges), to avoid the spoofing footgun
// the security rules flag.
//
// Uses net.SplitHostPort so IPv6 addresses like "[::1]:8080" parse
// correctly (LastIndex on ":" mangles them).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr without a port (rare, but possible behind some
		// transports / tests) -- fall back to the raw value.
		host = r.RemoteAddr
	}
	if isTrustedProxy(host) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if comma := strings.Index(xff, ","); comma > 0 {
				return strings.TrimSpace(xff[:comma])
			}
			return strings.TrimSpace(xff)
		}
	}
	return host
}

// isTrustedProxy returns true when host is loopback (v4 or v6) or in a
// private range. Uses net.IP.IsPrivate which correctly bounds the
// 172.16.0.0/12 subnet (the previous strings-prefix check trusted all
// of 172.0.0.0/8, opening X-Forwarded-For spoofing from any 172.x).
func isTrustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// isTrustedProxyRequest is the *http.Request convenience wrapper. Pulls
// the host out of RemoteAddr the same way clientIP does so the X-
// Forwarded-Proto trust gate matches the X-Forwarded-For trust gate.
func isTrustedProxyRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return isTrustedProxy(host)
}

// ipLimiter is a refill-bucket per IP. Burst tokens fill at refill/sec.
// Buckets that have not seen a request inside the idle window are
// evicted on the next .allow() call so a botnet or IPv6 spray cannot
// inflate the map until the daemon OOMs.
type ipLimiter struct {
	burst        int
	refill       float64
	idleEvictAge time.Duration
	maxBuckets   int
	mu           sync.Mutex
	buckets      map[string]*bucket
	lastSweep    time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(burst int, refill float64) *ipLimiter {
	return &ipLimiter{
		burst:        burst,
		refill:       refill,
		buckets:      map[string]*bucket{},
		idleEvictAge: 10 * time.Minute,
		maxBuckets:   50_000,
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.maybeSweepLocked(now)
	b, ok := l.buckets[ip]
	if !ok {
		// Hard ceiling: refuse the request rather than admit one more
		// unbounded entry to the map. Once the sweep has freed older
		// IPs, the limiter accepts new ones again.
		if len(l.buckets) >= l.maxBuckets {
			return false
		}
		b = &bucket{tokens: float64(l.burst), last: now}
		l.buckets[ip] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.refill
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// maybeSweepLocked evicts buckets that have not seen a request inside
// idleEvictAge. Runs at most every minute so a hot path doesn't pay
// a full-map walk on every call.
func (l *ipLimiter) maybeSweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	cutoff := now.Add(-l.idleEvictAge)
	for ip, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
}
