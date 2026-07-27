package auth

import "testing"

// TestVerifyPasswordEnforcesTheArgon2Floor pins a fix that landed on only one
// of two verifiers.
//
// There were two independent copies of the argon2id PHC parser:
// internal/server/password.go (used by the LEGACY env-var BasicAuth) and this
// one (used by VerifyPassword, which is the users-table path everything
// actually goes through now). The 2026-07-07 audit added an OWASP cost floor
// and a parameter range check to the server copy and logged it as fixed. This
// copy never got either, so the verifier everyone uses would happily recompute
// with whatever m/t/p a stored hash carried.
//
// EnsureAdminWithPHC writes an operator-supplied PHC straight from
// REACTOR_BASIC_AUTH_PASSWORD_SHA256 into password_phc and re-syncs it on every
// boot, so a weak or malformed env value reaches this parser directly.
func TestVerifyPasswordEnforcesTheArgon2Floor(t *testing.T) {
	t.Parallel()

	// A correctly-formed hash at a GPU-crackable cost. Verifying the right
	// password against it must still fail: fail closed, make the operator
	// re-hash, rather than silently accept a downgraded KDF.
	weak := EncodeArgon2idPHCForTest("hunter2", []byte("0123456789abcdef"), 1024, 1, 1)
	if VerifyPassword(weak, "hunter2") {
		t.Fatal("a below-floor argon2id hash (m=1MiB, t=1) was ACCEPTED; the KDF can be silently downgraded to GPU-crackable")
	}

	// The default cost must of course still work.
	strong, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(strong, "hunter2") {
		t.Fatal("the default HashPassword output was rejected by VerifyPassword")
	}
	if VerifyPassword(strong, "wrong") {
		t.Fatal("a wrong password was accepted")
	}
}

// TestVerifyPasswordDoesNotPanicOnMalformedParams pins an availability bug:
// argon2.IDKey PANICS when time < 1 or threads < 1 (x/crypto/argon2 argon2.go).
// This parser read m/t/p with fmt.Sscanf and validated none of them, so a
// typo'd env hash took down the login handler on every attempt instead of
// being rejected.
func TestVerifyPasswordDoesNotPanicOnMalformedParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stored string
	}{
		{"zero time", "$argon2id$v=19$m=65536,t=0,p=2$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"},
		{"zero parallelism", "$argon2id$v=19$m=65536,t=3,p=0$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"},
		{"missing params", "$argon2id$v=19$$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"},
		{"garbage param", "$argon2id$v=19$m=abc,t=3,p=2$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"},
		{"unknown param", "$argon2id$v=19$m=65536,t=3,p=2,k=9$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("VerifyPassword PANICKED on a malformed stored hash (%v); a typo in REACTOR_BASIC_AUTH_PASSWORD_SHA256 would take down every login", rec)
				}
			}()
			if VerifyPassword(tc.stored, "anything") {
				t.Fatal("a malformed stored hash was accepted")
			}
		})
	}
}
