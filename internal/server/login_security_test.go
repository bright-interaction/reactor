package server

import (
	"testing"
	"time"
)

func TestSafeNextPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/runs", "/runs"},
		{"/", "/"},
		{"", "/"},
		{"  /credentials  ", "/credentials"},
		{"//evil.com", "/"},       // protocol-relative open redirect
		{"/\\evil.com", "/"},      // backslash-smuggled
		{"https://evil.com", "/"}, // absolute
		{"javascript:alert(1)", "/"},
	}
	for _, c := range cases {
		if got := safeNextPath(c.in); got != c.want {
			t.Errorf("safeNextPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoginThrottleLocksAfterMaxFailures(t *testing.T) {
	tr := newLoginThrottle()
	now := time.Unix(1_700_000_000, 0)
	tr.now = func() time.Time { return now }
	key := "1.2.3.4|alice"

	for i := 0; i < tr.maxFailures-1; i++ {
		tr.recordFailure(key)
		if tr.locked(key) {
			t.Fatalf("locked too early after %d failures", i+1)
		}
	}
	tr.recordFailure(key) // crosses the threshold
	if !tr.locked(key) {
		t.Fatal("should be locked after max failures")
	}

	// A different key is unaffected (no global lockout).
	if tr.locked("1.2.3.4|bob") {
		t.Fatal("unrelated account should not be locked")
	}

	// Cooldown elapses -> unlocked again.
	now = now.Add(16 * time.Minute)
	if tr.locked(key) {
		t.Fatal("should be unlocked after cooldown")
	}

	// A success resets the counter.
	tr.recordFailure(key)
	tr.reset(key)
	if tr.locked(key) {
		t.Fatal("reset should clear the counter")
	}
}
