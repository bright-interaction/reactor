package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bright-interaction/reactor/internal/credentials"
)

// stubRotator implements CredentialRotator with programmable capabilities.
type stubRotator struct {
	canAuto      bool
	mintsLocally bool
	unknown      bool
	rotateCalls  int
}

func (s *stubRotator) RotateOne(_ context.Context, _ string) error {
	s.rotateCalls++
	return nil
}

func (s *stubRotator) ProviderCapabilities(_ string) (bool, bool, error) {
	if s.unknown {
		return false, false, errors.New("unknown provider")
	}
	return s.canAuto, s.mintsLocally, nil
}

func newGuardServer(rot CredentialRotator) *Server {
	return &Server{
		Rotator: rot,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestCredentialCreateRefusesDestructiveProvider pins the fix for the worst
// product defect in the 2026-07-25 audit. shared-secret was the FIRST <option>
// and therefore the default, and it MINTS a new random value locally. An
// operator added a real Stripe key with the select untouched, clicked "Rotate
// now", and the sk_live_ key was overwritten with random hex while the audit
// trail recorded rotate.success and Stripe still held the old key. Payments
// break silently and the original is unrecoverable.
//
// The refusal happens before the repo or vault is touched, so an unwired Server
// is sufficient here.
func TestRotationGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		rot        *stubRotator
		autoRotate bool
		ackReplace bool
		wantReason string // substring; "" means the combination must be allowed
	}{
		{
			// The Stripe case: value-replacing provider, no acknowledgement.
			name:       "destructive provider without acknowledgement is refused",
			rot:        &stubRotator{canAuto: true, mintsLocally: true},
			wantReason: "REPLACES the stored value",
		},
		{
			// Not a wall: an HMAC signing key Reactor itself issues is a
			// legitimate use of shared-secret.
			name:       "destructive provider with acknowledgement is allowed",
			rot:        &stubRotator{canAuto: true, mintsLocally: true},
			ackReplace: true,
		},
		{
			name:       "auto-rotate on a reminder-only provider is refused",
			rot:        &stubRotator{canAuto: false},
			autoRotate: true,
			wantReason: "cannot rotate automatically",
		},
		{
			name: "reminder-only provider without auto-rotate is allowed",
			rot:  &stubRotator{canAuto: false},
		},
		{
			name: "roll-at-source provider is allowed",
			rot:  &stubRotator{canAuto: true},
		},
		{
			name:       "auto-rotate on a roll-at-source provider is allowed",
			rot:        &stubRotator{canAuto: true},
			autoRotate: true,
		},
		{
			name:       "unknown provider is refused",
			rot:        &stubRotator{unknown: true},
			wantReason: "unknown provider",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newGuardServer(tc.rot)
			got := s.rotationGuard("p", tc.autoRotate, tc.ackReplace)
			if tc.wantReason == "" {
				if got != "" {
					t.Fatalf("combination should be allowed, got refusal: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantReason) {
				t.Fatalf("refusal = %q, want it to mention %q", got, tc.wantReason)
			}
		})
	}
}

// TestRotationGuardIsInertWhenRotationUnwired keeps a read-only deployment
// (no Rotator) from being unable to add credentials at all.
func TestRotationGuardIsInertWhenRotationUnwired(t *testing.T) {
	t.Parallel()
	s := newGuardServer(nil)
	if got := s.rotationGuard("shared-secret", true, false); got != "" {
		t.Fatalf("guard should be inert without a Rotator, got: %s", got)
	}
}

// TestCredentialNewFormDefaultsToManual pins the ordering. The default is
// whichever <option> comes first, and a destructive default is how the Stripe
// key got overwritten. manual is correct for every externally issued API key,
// which is what the ~40-service preset catalog is full of.
func TestCredentialNewFormDefaultsToManual(t *testing.T) {
	t.Parallel()
	html := credentialNewBody("", "")
	i := strings.Index(html, `<option value="manual"`)
	if i < 0 {
		t.Fatal("manual option missing")
	}
	for _, other := range []string{"shared-secret", "cloudflare", "aws-iam"} {
		j := strings.Index(html, `<option value="`+other+`"`)
		if j >= 0 && j < i {
			t.Fatalf("%q precedes manual, making it the default; a destructive default is the bug", other)
		}
	}
	if !strings.Contains(html, `name="confirm_replace"`) {
		t.Fatal("the acknowledgement checkbox for value-replacing providers is missing")
	}
}

// TestRotateHintDescribesTheActualEffect pins the copy next to the button. The
// hint used to claim it "runs the provider's mint -> deliver -> audit pipeline"
// for every provider, including reminder-only ones where nothing happens.
func TestRotateHintDescribesTheActualEffect(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		rot      *stubRotator
		want     string
		mintsLoc bool
	}{
		{"reminder only", &stubRotator{canAuto: false}, "records a reminder only", false},
		{"local mint", &stubRotator{canAuto: true, mintsLocally: true}, "NEW RANDOM secret", true},
		{"rolls at source", &stubRotator{canAuto: true}, "rolls the credential at its source", false},
		{"unknown provider", &stubRotator{unknown: true}, "unknown provider", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newGuardServer(tc.rot)
			hint, mintsLoc := s.rotateHint("p")
			if !strings.Contains(hint, tc.want) {
				t.Fatalf("hint = %q, want it to mention %q", hint, tc.want)
			}
			if mintsLoc != tc.mintsLoc {
				t.Fatalf("mintsLocally = %v, want %v", mintsLoc, tc.mintsLoc)
			}
		})
	}
}

// TestCredentialDetailWarnsBeforeDestructiveRotate closes the loop on the button
// itself: the primary-styled "Rotate now" fired with no confirmation at all.
func TestCredentialDetailWarnsBeforeDestructiveRotate(t *testing.T) {
	t.Parallel()
	c := credentials.Credential{ID: "stripe-key", Name: "stripe-key", Provider: "shared-secret"}

	destructive := credentialDetailBody(c, nil, nil, "",
		rotateView{hint: "generates a NEW RANDOM secret", mintsLocally: true})
	if !strings.Contains(destructive, "data-confirm=") {
		t.Fatal("a value-replacing rotation must be confirmed before it discards the secret")
	}
	if !strings.Contains(destructive, "cannot be recovered") {
		t.Fatal("the confirmation should name the consequence, not just ask 'are you sure'")
	}

	safe := credentialDetailBody(c, nil, nil, "",
		rotateView{hint: "rolls the credential at its source", mintsLocally: false})
	if strings.Contains(safe, "data-confirm=\"Provider") {
		t.Fatal("a roll-at-source rotation should not nag; confirmations lose meaning if everything has one")
	}
}

// TestCredentialDetailShowsRotateOutcome pins the flash. "Rotate now" used to
// 303 back with NO message, so on its most common configuration (manual, which
// the preset script selects for every catalog API-key service) the product's
// headline button did nothing observable and an operator could not tell that
// apart from a real rotation.
func TestCredentialDetailShowsRotateOutcome(t *testing.T) {
	t.Parallel()
	const msg = "Recorded a rotation reminder."
	html := credentialDetailBody(credentials.Credential{ID: "c", Name: "c", Provider: "manual"},
		nil, nil, msg, rotateView{hint: "records a reminder only"})
	if !strings.Contains(html, msg) {
		t.Fatal("the rotation outcome must be shown to the operator, not only written to the audit trail")
	}
}
