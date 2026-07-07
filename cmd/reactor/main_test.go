package main

import "testing"

func TestRedactDB(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		// previously panicked on the 10-char "postgres:/" because the old
		// implementation indexed [:11] under a len > 9 guard.
		{"postgres:/", "postgres:/"},
		{"postgres", "postgres"},
		{"", ""},
		{"sqlite://./reactor.db", "sqlite://./reactor.db"},
		{"postgres://user:pass@host/db", "postgres://[redacted]"},
		{"postgresql://user:pass@host/db", "postgres://[redacted]"},
	}
	for _, tc := range cases {
		got := redactDB(tc.in)
		if got != tc.want {
			t.Fatalf("redactDB(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
