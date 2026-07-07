package notifier

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip           string
		allowPrivate bool
		blocked      bool
	}{
		{"169.254.169.254", true, true},  // cloud metadata: blocked even when private allowed
		{"169.254.169.254", false, true}, // and blocked by default
		{"127.0.0.1", false, true},       // loopback blocked by default
		{"127.0.0.1", true, false},       // ...allowed when operator opts in
		{"10.1.2.3", false, true},        // RFC1918 blocked by default
		{"10.1.2.3", true, false},        // ...allowed on opt-in
		{"192.168.1.5", false, true},
		{"0.0.0.0", true, true},   // unspecified always blocked
		{"::1", false, true},      // IPv6 loopback
		{"fe80::1", true, true},   // IPv6 link-local always blocked
		{"8.8.8.8", false, false}, // public always allowed
		{"1.1.1.1", true, false},  // public
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", c.ip)
		}
		if got := isBlockedIP(ip, c.allowPrivate); got != c.blocked {
			t.Errorf("isBlockedIP(%s, allowPrivate=%v) = %v, want %v", c.ip, c.allowPrivate, got, c.blocked)
		}
	}
}
