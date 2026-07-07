// Package vault is the public credential-vault API that AI-generated
// workflow code imports.
//
// Workflow code calls vault.MustGet("stripe-key"). The returned Secret
// stringifies to "[REDACTED]" through every fmt verb, JSON encoder, slog
// handler, and text marshaller, so a buggy log statement cannot leak.
//
// In week 2 this package binds straight onto an internal Store via Bind.
// In week 3 it RPCs the host supervisor over the JSON-lines pipe so the
// workflow subprocess never holds DB credentials.
package vault

import (
	"context"
	"errors"
	"sync/atomic"
)

// Secret is a credential value. Plaintext is reachable only via Reveal;
// every other surface redacts.
type Secret interface {
	Reveal() []byte
	Fingerprint() string
	String() string
}

// Resolver is what the SDK calls into. The runtime supplies the
// implementation via Bind at boot.
type Resolver interface {
	Get(ctx context.Context, id string) (Secret, error)
}

var bound atomic.Pointer[Resolver]

// ErrNotBound indicates the SDK was used before the runtime called Bind.
// Surfaces clearly in unit tests that forget to wire a fake.
var ErrNotBound = errors.New("reactor/sdk/vault: no resolver bound (call vault.Bind from your runtime)")

// Bind installs the resolver. The runtime calls this once at startup.
// Tests use BindFunc.
func Bind(r Resolver) {
	bound.Store(&r)
}

// BindFunc is a convenience for tests: wraps a function as a Resolver.
func BindFunc(get func(ctx context.Context, id string) (Secret, error)) {
	Bind(funcResolver(get))
}

// MustGet panics if the resolver is unbound or the credential is missing.
// Workflow code calls this at step boundaries; the runtime's panic recovery
// turns the panic into a permanent step failure.
func MustGet(id string) Secret {
	v, err := Get(context.Background(), id)
	if err != nil {
		panic(err)
	}
	return v
}

// Get returns the credential or an error. Use this in tests + paths where
// MustGet's panic is not appropriate.
func Get(ctx context.Context, id string) (Secret, error) {
	rp := bound.Load()
	if rp == nil {
		return nil, ErrNotBound
	}
	return (*rp).Get(ctx, id)
}

type funcResolver func(ctx context.Context, id string) (Secret, error)

func (f funcResolver) Get(ctx context.Context, id string) (Secret, error) { return f(ctx, id) }
