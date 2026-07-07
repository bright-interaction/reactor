package auth

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/bright-interaction/reactor/internal/migrate"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	_ "modernc.org/sqlite"
)

// newTestStoreMFA is newTestStore with a fixed MFA key + a test relying party,
// so TOTP encryption and passkey construction are exercised.
func newTestStoreMFA(t *testing.T) (*Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "a.db")
	url := "sqlite://" + dbPath
	silent := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := migrate.Up(context.Background(), silent, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	key := DeriveMFAKey([]byte("test-master-key-0123456789abcdef"))
	wa, err := NewWebAuthn("example.com", "Reactor Test", []string{"https://example.com"})
	if err != nil {
		t.Fatalf("webauthn: %v", err)
	}
	return New(db, EngineSQLite, WithMFAKey(key), WithWebAuthn(wa)), func() { db.Close() }
}

func TestDeriveMFAKey(t *testing.T) {
	t.Parallel()
	master := []byte("some-master-key-material-abcdef01")
	k1 := DeriveMFAKey(master)
	k2 := DeriveMFAKey(master)
	if len(k1) != 32 {
		t.Fatalf("key len = %d, want 32", len(k1))
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("derivation not deterministic")
	}
	if bytes.Equal(k1, DeriveMFAKey([]byte("different-master-key-material-02"))) {
		t.Fatal("different master keys derived the same MFA key")
	}
	if DeriveMFAKey(nil) != nil {
		t.Fatal("empty master should derive nil (TOTP disabled)")
	}
}

func TestSecretEncryptRoundtripAndTamper(t *testing.T) {
	t.Parallel()
	s, done := newTestStoreMFA(t)
	defer done()
	plain := []byte("JBSWY3DPEHPK3PXP")
	sealed, err := s.encryptSecret(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.decryptSecret(sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip: got %q err %v", got, err)
	}
	// Tampering flips the GCM tag -> Open must fail.
	bad := []byte(sealed)
	bad[len(bad)-1] ^= 0x01
	if _, err := s.decryptSecret(string(bad)); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
	// A store without the key cannot decrypt.
	noKey := New(s.db, EngineSQLite)
	if _, err := noKey.decryptSecret(sealed); err != ErrMFAKeyMissing {
		t.Fatalf("no-key decrypt err = %v, want ErrMFAKeyMissing", err)
	}
}

func TestVerifyTOTPCodeReplayGuard(t *testing.T) {
	t.Parallel()
	// Pure-function test of the replay guard: a step at or below minStep is
	// never accepted, so a code cannot be spent twice.
	secret := "JBSWY3DPEHPK3PXP"
	now := time.Now().UTC()
	step := now.Unix() / totpPeriod
	code, err := totp.GenerateCodeCustom(secret, now, totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	gotStep, ok := verifyTOTPCode(secret, code, -1)
	if !ok || gotStep != step {
		t.Fatalf("fresh verify: ok=%v step=%d want %d", ok, gotStep, step)
	}
	if _, ok := verifyTOTPCode(secret, code, step); ok {
		t.Fatal("replayed code at consumed step was accepted")
	}
	if _, ok := verifyTOTPCode(secret, "000000", -1); ok {
		// Vanishingly unlikely to be the real code; guards the wrong-code path.
		t.Fatal("obviously wrong code accepted")
	}
}

func TestTOTPEnrollConfirmVerify(t *testing.T) {
	t.Parallel()
	s, done := newTestStoreMFA(t)
	defer done()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", "secret-pass", RoleAdmin)
	u, _ := s.GetUser(ctx, uid)

	secret, url, err := s.BeginTOTPEnrollment(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" || url == "" {
		t.Fatal("empty secret/url")
	}
	// Not a live factor until confirmed.
	if ok, _ := s.HasConfirmedTOTP(ctx, uid); ok {
		t.Fatal("unconfirmed TOTP counted as confirmed")
	}

	opts := totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
	now := time.Now().UTC()
	// Confirm with the previous step's code so the current step stays free for verify.
	confirmCode, _ := totp.GenerateCodeCustom(secret, now.Add(-totpPeriod*time.Second), opts)
	if err := s.ConfirmTOTP(ctx, uid, confirmCode); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if ok, _ := s.HasConfirmedTOTP(ctx, uid); !ok {
		t.Fatal("confirmed TOTP not reflected")
	}
	// Re-enroll must refuse now.
	if _, _, err := s.BeginTOTPEnrollment(ctx, u); err != ErrTOTPAlreadyEnabled {
		t.Fatalf("re-enroll err = %v, want ErrTOTPAlreadyEnabled", err)
	}

	verifyCode, _ := totp.GenerateCodeCustom(secret, now, opts)
	ok, err := s.VerifyTOTP(ctx, uid, verifyCode)
	if err != nil || !ok {
		t.Fatalf("verify: ok=%v err=%v", ok, err)
	}
	// Same code again -> replay guard rejects.
	if ok, _ := s.VerifyTOTP(ctx, uid, verifyCode); ok {
		t.Fatal("replayed TOTP code accepted at DB level")
	}
	if ok, _ := s.VerifyTOTP(ctx, uid, "000000"); ok {
		t.Fatal("wrong TOTP code accepted")
	}
	if err := s.DisableTOTP(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasConfirmedTOTP(ctx, uid); ok {
		t.Fatal("TOTP still confirmed after disable")
	}
}

func TestRecoveryCodesOneTime(t *testing.T) {
	t.Parallel()
	s, done := newTestStoreMFA(t)
	defer done()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", "secret-pass", RoleAdmin)

	codes, err := s.GenerateRecoveryCodes(ctx, uid)
	if err != nil || len(codes) != recoveryCodeCount {
		t.Fatalf("generate: %d codes err %v", len(codes), err)
	}
	if n, _ := s.CountUnusedRecoveryCodes(ctx, uid); n != recoveryCodeCount {
		t.Fatalf("unused = %d, want %d", n, recoveryCodeCount)
	}
	// Consume one (with dashes) -> success; again -> already used.
	if ok, _ := s.ConsumeRecoveryCode(ctx, uid, codes[0]); !ok {
		t.Fatal("first consume failed")
	}
	if ok, _ := s.ConsumeRecoveryCode(ctx, uid, codes[0]); ok {
		t.Fatal("recovery code consumed twice")
	}
	// Same code without dashes must also be rejected (already used).
	if ok, _ := s.ConsumeRecoveryCode(ctx, uid, normalizeRecoveryCode(codes[0])); ok {
		t.Fatal("normalized form of used code accepted")
	}
	if n, _ := s.CountUnusedRecoveryCodes(ctx, uid); n != recoveryCodeCount-1 {
		t.Fatalf("unused after consume = %d, want %d", n, recoveryCodeCount-1)
	}
	if ok, _ := s.ConsumeRecoveryCode(ctx, uid, "zzzz-zzzz-zzzz-zzzz"); ok {
		t.Fatal("unknown code accepted")
	}
	// Regenerating invalidates the old batch.
	if _, err := s.GenerateRecoveryCodes(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ConsumeRecoveryCode(ctx, uid, codes[1]); ok {
		t.Fatal("old code still valid after regeneration")
	}
}

func TestChallengeSingleUsePurposeAndExpiry(t *testing.T) {
	t.Parallel()
	s, done := newTestStoreMFA(t)
	defer done()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", "secret-pass", RoleAdmin)

	sd := &webauthn.SessionData{Challenge: "abc123", UserID: []byte(uid)}
	id, err := s.putChallenge(ctx, uid, purposeLogin, sd)
	if err != nil {
		t.Fatal(err)
	}
	// Purpose mismatch -> not found.
	if _, _, err := s.takeChallenge(ctx, id, purposeStepUp); err != ErrChallengeNotFound {
		t.Fatalf("purpose mismatch err = %v", err)
	}
	// Correct purpose -> found once.
	got, chUser, err := s.takeChallenge(ctx, id, purposeLogin)
	if err != nil || chUser != uid || got.Challenge != "abc123" {
		t.Fatalf("take: user=%s err=%v sd=%+v", chUser, err, got)
	}
	// Second take -> consumed.
	if _, _, err := s.takeChallenge(ctx, id, purposeLogin); err != ErrChallengeNotFound {
		t.Fatalf("second take err = %v, want ErrChallengeNotFound", err)
	}

	// Expired challenge is consumed but reported expired.
	expired := formatTime(time.Now().UTC().Add(-time.Hour))
	if _, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO webauthn_challenges (id, user_id, purpose, session_data, expires_at) VALUES ($1,$2,$3,$4,$5)`),
		"wch_expired", uid, purposeLogin, "{}", expired); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.takeChallenge(ctx, "wch_expired", purposeLogin); err != ErrChallengeExpired {
		t.Fatalf("expired take err = %v, want ErrChallengeExpired", err)
	}
}

func TestSessionMFAPendingAndElevation(t *testing.T) {
	t.Parallel()
	s, done := newTestStoreMFA(t)
	defer done()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", "secret-pass", RoleAdmin)

	cookie, err := s.CreatePendingSession(ctx, uid, "ua", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, st, err := s.ResolveSessionWithState(ctx, cookie)
	if err != nil || u.ID != uid {
		t.Fatalf("resolve: %v", err)
	}
	if !st.MFAPending {
		t.Fatal("pending session not reported MFAPending")
	}
	if st.IsElevated() {
		t.Fatal("pending session should not be elevated")
	}
	// Clearing pending also mints a step-up window.
	if err := s.ClearSessionMFAPending(ctx, cookie); err != nil {
		t.Fatal(err)
	}
	_, st, _ = s.ResolveSessionWithState(ctx, cookie)
	if st.MFAPending {
		t.Fatal("still pending after clear")
	}
	if !st.IsElevated() {
		t.Fatal("not elevated after clear")
	}
	// A regular session is neither pending nor elevated.
	plain, _ := s.CreateSession(ctx, uid, "", "", time.Hour)
	_, st, _ = s.ResolveSessionWithState(ctx, plain)
	if st.MFAPending || st.IsElevated() {
		t.Fatalf("plain session state = %+v", st)
	}
	if err := s.MarkSessionElevated(ctx, plain, StepUpWindow); err != nil {
		t.Fatal(err)
	}
	_, st, _ = s.ResolveSessionWithState(ctx, plain)
	if !st.IsElevated() {
		t.Fatal("not elevated after MarkSessionElevated")
	}
}

func TestPasskeyStorageAndHasMFA(t *testing.T) {
	t.Parallel()
	s, done := newTestStoreMFA(t)
	defer done()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", "secret-pass", RoleAdmin)

	if ok, _ := s.HasMFA(ctx, uid); ok {
		t.Fatal("user with no factor reported HasMFA")
	}
	cred := &webauthn.Credential{
		ID:        []byte("credential-id-bytes"),
		PublicKey: []byte("fake-cose-public-key"),
		Authenticator: webauthn.Authenticator{
			SignCount:    3,
			CloneWarning: false,
		},
	}
	cred.Flags.BackupState = true
	if err := s.insertCredential(ctx, uid, cred, "My Yubikey"); err != nil {
		t.Fatal(err)
	}
	// Duplicate authenticator refused.
	if err := s.insertCredential(ctx, uid, cred, "dup"); err != ErrPasskeyExists {
		t.Fatalf("dup insert err = %v, want ErrPasskeyExists", err)
	}
	if ok, _ := s.HasMFA(ctx, uid); !ok {
		t.Fatal("passkey not reflected in HasMFA")
	}
	factors, err := s.ListMFAFactors(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(factors.Passkeys) != 1 || factors.Passkeys[0].Name != "My Yubikey" || !factors.Passkeys[0].BackedUp {
		t.Fatalf("factors = %+v", factors)
	}
	rowID := factors.Passkeys[0].ID
	if err := s.RenameWebAuthnCredential(ctx, uid, rowID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	// Delete scoped to a different user must not remove it.
	other, _ := s.CreateUser(ctx, "bob", "secret-pass", RoleMember)
	if err := s.DeleteWebAuthnCredential(ctx, other, rowID); err != ErrNotFound {
		t.Fatalf("cross-user delete err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteWebAuthnCredential(ctx, uid, rowID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasMFA(ctx, uid); ok {
		t.Fatal("passkey still present after delete")
	}
}

func TestTOTPWithoutKeyRefuses(t *testing.T) {
	t.Parallel()
	// A store built without WithMFAKey must refuse TOTP but not panic.
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", "secret-pass", RoleAdmin)
	u, _ := s.GetUser(ctx, uid)
	if _, _, err := s.BeginTOTPEnrollment(ctx, u); err != ErrMFAKeyMissing {
		t.Fatalf("enroll without key err = %v, want ErrMFAKeyMissing", err)
	}
}
