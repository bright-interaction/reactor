package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/hkdf"
)

// ErrMFAKeyMissing is returned by TOTP operations when the Store was built
// without WithMFAKey. Passkeys need no server secret, so they still work; TOTP
// does, because its shared secret must be recoverable to compute codes.
var ErrMFAKeyMissing = errors.New("auth: TOTP is unavailable (no MFA encryption key configured)")

// ErrWebAuthnMissing is returned by passkey operations when the Store was
// built without WithWebAuthn (no relying-party domain configured).
var ErrWebAuthnMissing = errors.New("auth: passkeys are unavailable (WebAuthn relying party not configured)")

// DeriveMFAKey derives the 32-byte AES-256-GCM key used to encrypt TOTP
// secrets from the daemon master key via HKDF-SHA256 with a distinct info
// label. This cryptographically separates the MFA key from the vault's data
// key (which derives from the same master key under a different label), so a
// compromise of one domain's derived key does not yield the other. Returns nil
// when the master key is empty, which leaves TOTP disabled.
func DeriveMFAKey(masterKey []byte) []byte {
	if len(masterKey) == 0 {
		return nil
	}
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, masterKey, nil, []byte("reactor-mfa-totp-secret-v1"))
	if _, err := io.ReadFull(r, out); err != nil {
		// HKDF over a fixed-length output only fails if the reader fails,
		// which sha256 does not. Treat as fatal-adjacent: return nil so TOTP
		// stays disabled rather than using a partial key.
		return nil
	}
	return out
}

// encryptSecret seals plaintext with AES-256-GCM under the Store's MFA key and
// returns base64(nonce || ciphertext || tag). A random 12-byte nonce is
// prepended. Refuses when no key is configured.
func (s *Store) encryptSecret(plain []byte) (string, error) {
	if len(s.mfaKey) != 32 {
		return "", ErrMFAKeyMissing
	}
	block, err := aes.NewCipher(s.mfaKey)
	if err != nil {
		return "", fmt.Errorf("auth: mfa cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("auth: mfa gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: mfa nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptSecret reverses encryptSecret.
func (s *Store) decryptSecret(b64 string) ([]byte, error) {
	if len(s.mfaKey) != 32 {
		return nil, ErrMFAKeyMissing
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("auth: mfa decode: %w", err)
	}
	block, err := aes.NewCipher(s.mfaKey)
	if err != nil {
		return nil, fmt.Errorf("auth: mfa cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: mfa gcm: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("auth: mfa ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: mfa open: %w", err)
	}
	return plain, nil
}

// NewWebAuthn builds a relying-party handle for the given RP id, display name,
// and permitted origins. rpID is the effective domain (e.g. "reactor.example.com",
// no scheme/port); origins are the full origins the browser will report (e.g.
// "https://reactor.example.com"). Returns an error when the config is invalid,
// which the caller surfaces so passkeys are simply left disabled.
func NewWebAuthn(rpID, displayName string, origins []string) (*webauthn.WebAuthn, error) {
	// Normalize to the ASCII-lowercased effective domain (see RPIDFromOrigin).
	rpID = strings.ToLower(strings.TrimSpace(rpID))
	if rpID == "" {
		return nil, errors.New("auth: WebAuthn RP ID is required")
	}
	if displayName == "" {
		displayName = "Reactor"
	}
	clean := make([]string, 0, len(origins))
	for _, o := range origins {
		if o = strings.TrimSpace(o); o != "" {
			clean = append(clean, o)
		}
	}
	if len(clean) == 0 {
		return nil, errors.New("auth: at least one WebAuthn origin is required")
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: displayName,
		RPOrigins:     clean,
	})
}

// RPIDFromOrigin extracts the effective domain (host without scheme/port) from
// a full origin URL, for auto-deriving the RP ID from a configured dashboard
// URL when REACTOR_WEBAUTHN_RP_ID is not set explicitly.
func RPIDFromOrigin(origin string) string {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return ""
	}
	// The WebAuthn RP ID must be the ASCII-lowercased effective domain: browsers
	// compute rpIdHash over the lowercased host, so a mixed-case value silently
	// fails navigator.credentials on compliant agents.
	return strings.ToLower(u.Hostname())
}
