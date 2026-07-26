// Package rotators owns the "mint a new credential value, deliver it to
// every consumer, audit the event" pipeline. Built deliberately small:
// one Provider interface, a registry, an HTTP delivery target, and a
// runner that ticks against credentials.ListNeedingRotation.
//
// Mirrors the proven shape from automations/dockyard/internal/handlers/
// vault_providers.go, ported to Reactor's vault.Store + credentials
// repo + credential_audit table. The dockyard runtime has been rotating
// 33 providers for months; Reactor ships with cloudflare + a generic
// random shared-secret provider for v1, and adds more via the Registry.
package rotators

import (
	"context"
	"fmt"
)

// Provider mints fresh credentials for a single upstream service. The
// runner calls Rotate(currentValue, meta), persists the returned new
// value through vault.Store, then drives delivery to each rotation
// target so consumers get the new key without a restart.
type Provider interface {
	// Name is the registry key (e.g. "cloudflare", "shared-secret").
	Name() string

	// CanAutoRotate reports whether Rotate is implemented. Validate-only
	// providers (GitHub PATs, OpenAI keys, etc.) return false; the runner
	// emits a "rotation due" reminder audit row instead of attempting
	// programmatic rotation.
	CanAutoRotate() bool

	// Rotate produces a new credential value. currentKey is the
	// plaintext of the current value (already decrypted by the caller).
	// meta is the credential's provider_meta JSON map. Implementations
	// must NOT log the new value; the runner is the only writer.
	Rotate(ctx context.Context, currentKey string, meta map[string]string) (newKey string, err error)

	// Validate reports whether the given key still works. Used pre- and
	// post-rotation to confirm upstream state.
	Validate(ctx context.Context, key string, meta map[string]string) (valid bool, err error)
}

// LocalMinter is an OPTIONAL Provider capability. Implement it and return true
// when Rotate mints a brand-new secret LOCALLY instead of rolling the credential
// at the issuing service.
//
// The distinction is the difference between rotation and destruction. Rolling at
// source (cloudflare, aws-iam) yields a value the service itself issued, so the
// integration keeps working. Minting locally (shared-secret) is correct only for
// a secret Reactor is the authority for, such as an HMAC signing key where
// Reactor controls both ends. Point it at an externally issued API key and it
// overwrites the real key with a value the service has never seen, the
// integration breaks silently, and the audit trail records rotate.success.
//
// It is an optional interface rather than a Provider method because Provider is
// a public extension point (Register is documented for runtime extension), so
// adding a required method would break third-party providers.
type LocalMinter interface {
	MintsValueLocally() bool
}

// MintsValueLocally reports whether rotating this provider REPLACES the stored
// value with a newly generated secret. Providers that do not implement
// LocalMinter are assumed to roll at source, which is the safe default for a
// warning gate: it never blocks a legitimate rotation, it only declines to warn
// about a provider that has not declared itself destructive.
func MintsValueLocally(p Provider) bool {
	lm, ok := p.(LocalMinter)
	return ok && lm.MintsValueLocally()
}

// Registry holds all built-in providers. Workflows can extend this at
// runtime via Register; default registry is populated by init() below.
var Registry = map[string]Provider{}

// Register installs a provider under its Name(). Idempotent.
func Register(p Provider) {
	if p == nil {
		return
	}
	Registry[p.Name()] = p
}

// Get returns the registered provider or an error if missing.
func Get(name string) (Provider, error) {
	p, ok := Registry[name]
	if !ok {
		return nil, fmt.Errorf("rotators: unknown provider %q", name)
	}
	return p, nil
}

func init() {
	Register(&CloudflareProvider{})
	Register(&SharedSecretProvider{})
	Register(&ManualProvider{})
	Register(&AWSIAMProvider{})
}
