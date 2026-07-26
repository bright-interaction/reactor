package rotators

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// SharedSecretProvider mints a fresh 32-byte random value, hex-encoded
// (64 chars). Used for inter-service HMAC shared secrets where both
// sides accept any opaque token; rotation is just "roll the bytes".
//
// Verification happens implicitly via the rotation targets: if the
// reload-secrets POST fails, the runner records the failure and the
// operator sees it in last_rotation_error. There is no upstream API
// to validate against.
type SharedSecretProvider struct{}

func (p *SharedSecretProvider) Name() string        { return "shared-secret" }
func (p *SharedSecretProvider) CanAutoRotate() bool { return true }

// MintsValueLocally implements LocalMinter. This provider generates a fresh
// random secret and stores it; nothing is rolled at any upstream service. Using
// it on an externally issued API key destroys that key, so the dashboard gates
// the choice behind an explicit acknowledgement.
func (p *SharedSecretProvider) MintsValueLocally() bool { return true }

// Rotate generates 32 bytes from crypto/rand and hex-encodes them.
func (p *SharedSecretProvider) Rotate(_ context.Context, _ string, _ map[string]string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Validate is a no-op for this provider: anything non-empty passes.
// The runner skips Validate when CanAutoRotate is true and the rotation
// path is "always mint fresh", but we satisfy the interface so external
// callers (CLI, future MCP tool) can call Validate without a type
// assertion.
func (p *SharedSecretProvider) Validate(_ context.Context, key string, _ map[string]string) (bool, error) {
	return key != "", nil
}

// ManualProvider is the catch-all for credentials that must be rotated
// by a human (GitHub PATs, OpenAI keys, anything without a programmatic
// rotation API). The runner emits a "rotation due" reminder audit row
// and surfaces last_rotation_error so an operator notices.
type ManualProvider struct{}

func (p *ManualProvider) Name() string        { return "manual" }
func (p *ManualProvider) CanAutoRotate() bool { return false }

func (p *ManualProvider) Rotate(_ context.Context, _ string, _ map[string]string) (string, error) {
	return "", fmt.Errorf("manual rotation required")
}

func (p *ManualProvider) Validate(_ context.Context, _ string, _ map[string]string) (bool, error) {
	return false, fmt.Errorf("manual provider does not validate")
}
