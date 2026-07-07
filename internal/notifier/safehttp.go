package notifier

import (
	"net"
	"net/http"

	"github.com/bright-interaction/reactor/internal/safehttp"
)

// ssrfSafeClient returns an *http.Client hardened against SSRF for
// operator-configured webhook URLs. See internal/safehttp for the shared
// implementation (connect-time IP check + no-redirects).
func ssrfSafeClient(allowPrivate bool) *http.Client {
	return safehttp.Client(allowPrivate)
}

// isBlockedIP reports whether an IP is in a range a webhook must never reach.
func isBlockedIP(ip net.IP, allowPrivate bool) bool {
	return safehttp.BlockedIP(ip, allowPrivate)
}
