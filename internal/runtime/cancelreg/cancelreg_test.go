package cancelreg

import (
	"context"
	"testing"
)

func TestRegistryCancel(t *testing.T) {
	r := New()
	cancelled := false
	r.Register("run_1", func() { cancelled = true })

	// Cancelling a registered run fires its func and reports true.
	if !r.Cancel("run_1") {
		t.Fatal("Cancel should report true for a registered run")
	}
	if !cancelled {
		t.Fatal("cancel func was not invoked")
	}

	// Unknown run -> false, no panic.
	if r.Cancel("run_missing") {
		t.Fatal("Cancel should report false for an unknown run")
	}

	// Deregister removes it.
	r.Register("run_2", func() {})
	r.Deregister("run_2")
	if r.Cancel("run_2") {
		t.Fatal("Cancel should report false after Deregister")
	}
}

func TestRegistryCancelAll(t *testing.T) {
	r := New()
	n := 0
	r.Register("a", func() { n++ })
	r.Register("b", func() { n++ })
	r.Register("c", func() { n++ })
	if got := r.CancelAll(); got != 3 {
		t.Fatalf("CancelAll returned %d, want 3", got)
	}
	if n != 3 {
		t.Fatalf("expected 3 cancel funcs invoked, got %d", n)
	}
	// A nil registry is safe.
	var nilReg *Registry
	if nilReg.CancelAll() != 0 {
		t.Fatal("nil registry CancelAll should be 0")
	}
}

func TestRegistryNilSafe(t *testing.T) {
	var r *Registry // nil
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Register("x", cancel) // must not panic
	r.Deregister("x")
	if r.Cancel("x") {
		t.Fatal("nil registry Cancel should be false")
	}
}
