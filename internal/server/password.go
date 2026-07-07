package server

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
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
	if strings.HasPrefix(stored, "$argon2id$") {
		params, salt, hash, err := parseArgon2idPHC(stored)
		if err != nil {
			// Fail closed; the operator will see Unauthorized on every
			// attempt and can re-hash. Better than silently accepting.
			return func(string) bool { return false }
		}
		// Parameter floor: reject a stored PHC whose cost is below the OWASP
		// minimum. The verifier recomputes with whatever m/t/p the PHC carries,
		// so a tampered or ancient weak hash would silently downgrade the KDF to
		// GPU-crackable. Fail closed so the operator re-hashes with the strong
		// defaults (`reactor setup` / `reactor hashpw`). The default is well
		// above this floor (m=64MiB, t=3).
		if params.m < minArgon2MemKiB || params.t < minArgon2Time {
			return func(string) bool { return false }
		}
		return func(plaintext string) bool {
			got := argon2.IDKey([]byte(plaintext), salt, params.t, params.m, params.p, uint32(len(hash)))
			return subtle.ConstantTimeCompare(got, hash) == 1
		}
	}
	wantHex := []byte(strings.ToLower(stored))
	return func(plaintext string) bool {
		gotHex := []byte(sha256Hex(plaintext))
		return subtle.ConstantTimeCompare(gotHex, wantHex) == 1
	}
}

// OWASP-minimum argon2id cost the verifier will accept. Below this a stored
// PHC is treated as invalid (fail closed) rather than silently downgrading.
const (
	minArgon2MemKiB uint32 = 19456 // 19 MiB
	minArgon2Time   uint32 = 2
)

type argon2idParams struct {
	m uint32
	t uint32
	p uint8
}

// parseArgon2idPHC splits a $argon2id$ PHC-encoded string into its
// parameters, salt, and hash bytes. Format:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<base64-salt>$<base64-hash>
//
// Base64 is the unpadded standard alphabet used by the reference
// implementation; we accept both padded and unpadded inputs for
// resilience.
func parseArgon2idPHC(s string) (argon2idParams, []byte, []byte, error) {
	parts := strings.Split(s, "$")
	// Leading empty segment from the leading $ -> 6 parts total.
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argon2idParams{}, nil, nil, errors.New("argon2id: bad PHC header")
	}
	if parts[2] != "v=19" {
		return argon2idParams{}, nil, nil, fmt.Errorf("argon2id: unsupported version %q", parts[2])
	}
	var p argon2idParams
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return argon2idParams{}, nil, nil, fmt.Errorf("argon2id: bad param %q", kv)
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return argon2idParams{}, nil, nil, fmt.Errorf("argon2id: parse param %q: %w", kv, err)
		}
		switch k {
		case "m":
			p.m = uint32(n)
		case "t":
			p.t = uint32(n)
		case "p":
			if n == 0 || n > 255 {
				return argon2idParams{}, nil, nil, fmt.Errorf("argon2id: p out of range")
			}
			p.p = uint8(n)
		default:
			return argon2idParams{}, nil, nil, fmt.Errorf("argon2id: unknown param %q", k)
		}
	}
	if p.m == 0 || p.t == 0 || p.p == 0 {
		return argon2idParams{}, nil, nil, errors.New("argon2id: missing required parameter")
	}
	salt, err := decodeBase64(parts[4])
	if err != nil {
		return argon2idParams{}, nil, nil, fmt.Errorf("argon2id: salt: %w", err)
	}
	hash, err := decodeBase64(parts[5])
	if err != nil {
		return argon2idParams{}, nil, nil, fmt.Errorf("argon2id: hash: %w", err)
	}
	return p, salt, hash, nil
}

func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
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
