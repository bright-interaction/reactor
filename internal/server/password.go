package server

import (
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/bright-interaction/reactor/internal/auth"
)

// newPasswordVerifier returns a constant-time password check closure.
//
// The stored field carries one of two shapes:
//
//   - argon2id PHC string: $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
//   - lowercase hex SHA-256 of the password (legacy v0.x)
//
// The closure sniffs the prefix and routes to the right comparator.
// SHA-256 is GPU-brute-forceable; new installs should use argon2id
// (generated via `reactor setup` or the standalone `reactor hashpw`
// helper). The legacy path stays so existing operators are not forced
// to rotate on upgrade.
func newPasswordVerifier(stored string) func(plaintext string) bool {
	stored = strings.TrimSpace(stored)
	// Delegate to internal/auth rather than keeping a second parser here.
	//
	// There used to be two independent copies of this logic, and the 2026-07-07
	// audit's argon2 cost floor landed on THIS one while the users-table
	// verifier in internal/auth (the path everything actually goes through)
	// never got it. One owner, one floor, no drift. auth.VerifyPassword sniffs
	// the same $argon2id$ / legacy-hex prefix this did.
	return func(plaintext string) bool {
		return auth.VerifyPassword(stored, plaintext)
	}
}

// EncodeArgon2idPHC produces the standard PHC string for storage. Used
// by `reactor setup` and `reactor hashpw` to mint hashes operators can
// paste into the env var.
func EncodeArgon2idPHC(password string, salt []byte, m, t uint32, p uint8) string {
	hash := argon2.IDKey([]byte(password), salt, t, m, p, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		m, t, p,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
}
