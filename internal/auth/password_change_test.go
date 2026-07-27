package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPasswordChangeAndReset covers the surface that did not exist at all.
//
// The dashboard shipped with no way to change or reset a password: /users
// offered create, role, tenant, enable, disable and delete, /account and
// /security had nothing, and the login page told users to "ask an admin to mint
// a fresh one for you via the Users page" for an action that page could not
// perform. The only recovery was delete-and-recreate, which drops the user's
// MFA factors and API tokens, and a user whose password leaked could not
// rotate it.
func TestPasswordChangeAndReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, closeDB := newTestStore(t)
	defer closeDB()

	id, err := s.CreateUser(ctx, "alice", "original-password", RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("self-service requires the current password", func(t *testing.T) {
		if err := s.ChangePassword(ctx, id, "wrong-password", "brand-new-password"); !errors.Is(err, ErrInvalidPassword) {
			t.Fatalf("ChangePassword with a WRONG current password returned %v, want ErrInvalidPassword; a stolen cookie alone must not be able to take over the account", err)
		}
		// And the original must still work.
		if _, err := s.Authenticate(ctx, "alice", "original-password"); err != nil {
			t.Fatalf("the original password stopped working after a failed change: %v", err)
		}
	})

	t.Run("rejects a short password", func(t *testing.T) {
		if err := s.SetPassword(ctx, id, "short"); !errors.Is(err, ErrPasswordTooShort) {
			t.Fatalf("SetPassword accepted a 5-character password: %v", err)
		}
	})

	t.Run("change rotates the password and kills live sessions", func(t *testing.T) {
		cookie, err := s.CreateSession(ctx, id, "ua", "127.0.0.1", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveSession(ctx, cookie); err != nil {
			t.Fatalf("precondition: session should resolve before the change: %v", err)
		}

		if err := s.ChangePassword(ctx, id, "original-password", "brand-new-password"); err != nil {
			t.Fatalf("ChangePassword with the correct current password failed: %v", err)
		}

		if _, err := s.Authenticate(ctx, "alice", "brand-new-password"); err != nil {
			t.Fatalf("the new password does not authenticate: %v", err)
		}
		if _, err := s.Authenticate(ctx, "alice", "original-password"); err == nil {
			t.Fatal("the OLD password still authenticates after a change")
		}
		if _, err := s.ResolveSession(ctx, cookie); err == nil {
			t.Fatal("a session minted before the password change still resolves; a rotated password must close existing access")
		}
	})

	t.Run("admin reset does not need the old password", func(t *testing.T) {
		if err := s.SetPassword(ctx, id, "admin-issued-password"); err != nil {
			t.Fatalf("admin SetPassword failed: %v", err)
		}
		if _, err := s.Authenticate(ctx, "alice", "admin-issued-password"); err != nil {
			t.Fatalf("the admin-issued password does not authenticate: %v", err)
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		if err := s.SetPassword(ctx, "usr_nope", "some-long-password"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SetPassword on a missing user returned %v, want ErrNotFound", err)
		}
	})
}
