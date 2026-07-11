// Package flarereport wires error reporting to the house Flare
// instance (Sentry wire protocol). Everything here is a no-op unless
// FLARE_DSN is set, so dev runs and self-hosts boot unchanged.
package flarereport

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

// InitFlare wires error reporting to the house Flare instance (Sentry-wire
// protocol) when FLARE_DSN is set in the environment. The DSN is injected by
// the Hephaestus flare-provision deploy step; without it this is a no-op so
// dev runs and self-hosts boot unchanged.
func InitFlare(service, release string) bool {
	dsn := os.Getenv("FLARE_DSN")
	if dsn == "" {
		return false
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:        dsn,
		Release:    release,
		ServerName: service,
	})
	if err != nil {
		slog.Warn("flare: error reporting disabled (sentry init failed)", "error", err)
		return false
	}
	slog.Info("flare: error reporting enabled", "service", service)
	startHeartbeat(service)
	installLogShipper(service)
	return true
}

// FlareRecoverer captures panics to Flare and renders a plain 500.
// Reactor mounts no other recovery middleware, so unlike the dockyard
// variant this one must NOT re-panic: with nothing above it a re-panic
// would reach net/http, which aborts the connection without a
// response. Mount it first so it wraps the whole chain. Safe to mount
// when InitFlare was a no-op: capture calls on an uninitialized hub do
// nothing.
func FlareRecoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					// net/http's sentinel for deliberately aborting a
					// response; swallowing it would break handlers
					// that panic with it on purpose.
					panic(rec)
				}
				hub := sentry.CurrentHub().Clone()
				hub.Scope().SetRequest(r)
				hub.RecoverWithContext(r.Context(), rec)
				hub.Flush(2 * time.Second)
				slog.Error("http: panic recovered", "method", r.Method, "path", r.URL.Path, "panic", rec)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CaptureErr reports a non-panic error to Flare. No-op when reporting is
// disabled. Use for errors that are handled but should page someone.
func CaptureErr(err error) {
	if err == nil {
		return
	}
	sentry.CaptureException(err)
}
