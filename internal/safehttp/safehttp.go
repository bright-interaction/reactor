// Package safehttp provides an *http.Client hardened against SSRF, shared by
// every daemon subsystem that dials an operator- or workflow-influenced URL
// (webhook notifiers, credential-rotation delivery). Centralising it means the
// guard cannot be applied to one HTTP client and forgotten on another.
package safehttp

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Client returns an *http.Client hardened against SSRF. Two layers:
//
//   - A Dialer.Control hook inspects the ACTUAL resolved IP right before
//     connect and refuses blocked addresses. Checking at connect time (not
//     before) also defeats DNS rebinding, where a name resolves to a public IP
//     on the first lookup and a private/metadata one on the connect lookup.
//   - CheckRedirect refuses to follow redirects, so a 302 to
//     http://169.254.169.254/ can't smuggle a request (or a rotated secret)
//     past the URL check.
//
// allowPrivate lets a self-hosted daemon reach loopback/RFC1918 internal
// services (credential rotation legitimately targets internal reload
// endpoints); link-local (incl. the 169.254.169.254 cloud-metadata endpoint),
// unspecified, and multicast are ALWAYS blocked regardless.
func Client(allowPrivate bool) *http.Client {
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("ssrf: bad address %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("ssrf: unresolved address %q", host)
			}
			if BlockedIP(ip, allowPrivate) {
				return fmt.Errorf("ssrf: refusing to connect to non-public address %s", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DialContext: d.DialContext},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("ssrf: redirects are not followed")
		},
	}
}

// BlockedIP reports whether an IP is in a range an outbound request must never
// reach. Link-local (which includes the 169.254.169.254 cloud-metadata
// endpoint), unspecified, and multicast are ALWAYS blocked: those are never a
// legitimate target and the metadata endpoint is the classic SSRF
// credential-theft prize. Loopback + private (RFC1918 + IPv6 ULA) are blocked
// by default but allowed when allowPrivate is set.
func BlockedIP(ip net.IP, allowPrivate bool) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if allowPrivate {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
