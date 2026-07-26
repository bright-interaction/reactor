package credentials

import (
	"context"
	"strings"
	"testing"
)

// TestTargetValidate is the single source of truth for which fields a delivery
// kind requires. The dashboard's add-target form and the rotation runner both
// call it, so they cannot drift into accepting different things (the runner used
// to carry its own per-case field checks inline).
func TestTargetValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		target  Target
		wantErr string // substring; "" means valid
	}{
		{"webhook complete", Target{Kind: "webhook", URL: "https://x/y", KeyName: "K", SecretID: "cred_a"}, ""},
		{"webhook without key_name", Target{Kind: "webhook", URL: "https://x/y", SecretID: "cred_a"}, "key_name"},
		{"webhook without secret_id", Target{Kind: "webhook", URL: "https://x/y", KeyName: "K"}, "secret_id"},
		{"webhook without url", Target{Kind: "webhook", KeyName: "K", SecretID: "cred_a"}, "url"},

		{"reload_endpoint complete", Target{Kind: "reload_endpoint", URL: "https://x/y", KeyName: "K", SecretID: "cred_a"}, ""},

		// file_write needs only a path: no key name, no auth secret.
		{"file_write complete", Target{Kind: "file_write", URL: "/etc/app/secret"}, ""},
		{"file_write without url", Target{Kind: "file_write"}, "url"},

		// dockyard_vault needs a token but no key name.
		{"dockyard_vault complete", Target{Kind: "dockyard_vault", URL: "https://d/entry", SecretID: "cred_tok"}, ""},
		{"dockyard_vault without secret_id", Target{Kind: "dockyard_vault", URL: "https://d/entry"}, "secret_id"},

		{"forgejo_secret complete", Target{Kind: "forgejo_secret", URL: "https://f/api", KeyName: "S", SecretID: "cred_tok"}, ""},
		{"github_secret complete", Target{Kind: "github_secret", URL: "https://api.github.com/repos/o/r", KeyName: "S", SecretID: "cred_pat"}, ""},

		{"negative grace", Target{Kind: "file_write", URL: "/p", GraceSeconds: -1}, "grace_seconds"},
		{"unknown kind", Target{Kind: "carrier-pigeon", URL: "/p"}, "unsupported target kind"},
		{"empty kind", Target{URL: "/p"}, "unsupported target kind"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.target.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestTargetKindsCoverEveryDeliveryKind guards the operator surface against the
// runner growing a kind the dashboard cannot offer. TargetKinds drives both the
// form's <select> and Validate, so a kind missing here is unreachable from the
// UI and rejected by validation even though delivery implements it.
func TestTargetKindsCoverEveryDeliveryKind(t *testing.T) {
	t.Parallel()
	// Mirrors the switch in rotators.deliverOne.
	implemented := []string{"webhook", "reload_endpoint", "file_write", "forgejo_secret", "github_secret", "dockyard_vault"}
	have := map[string]bool{}
	for _, k := range TargetKinds {
		have[k.Kind] = true
	}
	for _, kind := range implemented {
		if !have[kind] {
			t.Fatalf("delivery implements %q but TargetKinds omits it, so no operator can configure it", kind)
		}
	}
	if len(TargetKinds) != len(implemented) {
		t.Fatalf("TargetKinds has %d entries, delivery implements %d; a kind offered but not implemented fails at rotation time", len(TargetKinds), len(implemented))
	}
}

// TestSetRotationTargetsRoundTrip is the data round-trip: targets written
// through the repo must come back off a fresh read. Before this setter existed,
// rotation_targets was reachable only by hand-written SQL, so every credential an
// operator could create had none and rotation delivered nowhere.
func TestSetRotationTargetsRoundTrip(t *testing.T) {
	t.Parallel()
	r, cleanup := newRepo(t)
	defer cleanup()
	ctx := context.Background()

	if err := r.Create(ctx, CreateParams{ID: "c1", Name: "c1", Service: "svc", Provider: "manual"}); err != nil {
		t.Fatal(err)
	}
	// A fresh credential starts with none, which is exactly the state that used
	// to be permanent.
	got, err := r.Get(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RotationTargets) != 0 {
		t.Fatalf("new credential should have no targets, got %d", len(got.RotationTargets))
	}

	want := []Target{
		{Kind: "webhook", URL: "https://a/hook", KeyName: "API_KEY", SecretID: "cred_hmac", GraceSeconds: 30},
		{Kind: "file_write", URL: "/etc/app/key"},
	}
	if err := r.SetRotationTargets(ctx, "c1", want); err != nil {
		t.Fatal(err)
	}
	got, err = r.Get(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RotationTargets) != 2 {
		t.Fatalf("got %d targets, want 2", len(got.RotationTargets))
	}
	if got.RotationTargets[0] != want[0] || got.RotationTargets[1] != want[1] {
		t.Fatalf("round-trip mismatch:\ngot  %+v\nwant %+v", got.RotationTargets, want)
	}

	// Removing one persists.
	if err := r.SetRotationTargets(ctx, "c1", want[:1]); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Get(ctx, "c1")
	if len(got.RotationTargets) != 1 || got.RotationTargets[0].Kind != "webhook" {
		t.Fatalf("after removal got %+v", got.RotationTargets)
	}

	// Clearing to empty persists as empty, not as a nil that reads back stale.
	if err := r.SetRotationTargets(ctx, "c1", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Get(ctx, "c1")
	if len(got.RotationTargets) != 0 {
		t.Fatalf("after clearing got %+v", got.RotationTargets)
	}
}

// TestSetRotationTargetsRejectsInvalid keeps a malformed target out of the
// column, where it would otherwise sit until a scheduler tick hours later failed
// on it with nobody watching.
func TestSetRotationTargetsRejectsInvalid(t *testing.T) {
	t.Parallel()
	r, cleanup := newRepo(t)
	defer cleanup()
	ctx := context.Background()
	if err := r.Create(ctx, CreateParams{ID: "c1", Name: "c1", Provider: "manual"}); err != nil {
		t.Fatal(err)
	}

	err := r.SetRotationTargets(ctx, "c1", []Target{
		{Kind: "webhook", URL: "https://ok", KeyName: "K", SecretID: "cred_a"},
		{Kind: "webhook", URL: "https://missing-key-name"},
	})
	if err == nil {
		t.Fatal("a malformed target must be rejected before the write")
	}
	if !strings.Contains(err.Error(), "target 2") {
		t.Fatalf("error should identify which target failed, got %v", err)
	}
	// And nothing partial was written.
	got, _ := r.Get(ctx, "c1")
	if len(got.RotationTargets) != 0 {
		t.Fatalf("rejected batch must not persist, got %+v", got.RotationTargets)
	}
}

func TestSetRotationTargetsUnknownCredential(t *testing.T) {
	t.Parallel()
	r, cleanup := newRepo(t)
	defer cleanup()
	if err := r.SetRotationTargets(context.Background(), "nope", nil); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
