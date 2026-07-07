package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeSecret struct{ v []byte }

func (f *fakeSecret) Reveal() []byte      { return f.v }
func (f *fakeSecret) Fingerprint() string { return "sha256:fakefake" }
func (f *fakeSecret) String() string      { return "[REDACTED]" }

func TestUnboundResolverErrors(t *testing.T) {
	// reset the global between subtests
	bound.Store(nil)
	if _, err := Get(context.Background(), "x"); !errors.Is(err, ErrNotBound) {
		t.Fatalf("got %v, want ErrNotBound", err)
	}
}

func TestBindFuncRoundTrip(t *testing.T) {
	BindFunc(func(ctx context.Context, id string) (Secret, error) {
		if id != "stripe-key" {
			return nil, errors.New("not found")
		}
		return &fakeSecret{v: []byte("sk_live_abc")}, nil
	})
	t.Cleanup(func() { bound.Store(nil) })

	s, err := Get(context.Background(), "stripe-key")
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Reveal()) != "sk_live_abc" {
		t.Fatalf("got %q", s.Reveal())
	}
	// fmt formatting must not leak.
	if got := fmt.Sprintf("%v", s); strings.Contains(got, "sk_live_abc") {
		t.Fatalf("leaked: %q", got)
	}
}

func TestMustGetPanicsOnMissing(t *testing.T) {
	BindFunc(func(ctx context.Context, id string) (Secret, error) {
		return nil, errors.New("missing")
	})
	t.Cleanup(func() { bound.Store(nil) })

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustGet should panic on resolver error")
		}
	}()
	_ = MustGet("anything")
}
