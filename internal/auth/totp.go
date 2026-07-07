package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

const (
	totpPeriod = 30 // seconds per code
	totpSkew   = 1  // accept the code from +/- 1 step to tolerate clock drift
	totpIssuer = "Reactor"
)

// ErrTOTPAlreadyEnabled is returned when starting enrollment while a confirmed
// TOTP secret already exists (the operator must disable it first).
var ErrTOTPAlreadyEnabled = errors.New("auth: TOTP is already enabled; disable it before re-enrolling")

// HasConfirmedTOTP reports whether the user has a confirmed TOTP factor.
func (s *Store) HasConfirmedTOTP(ctx context.Context, userID string) (bool, error) {
	const q = `SELECT COUNT(*) FROM totp_secrets WHERE user_id = $1 AND confirmed_at IS NOT NULL`
	var n int
	if err := s.db.QueryRowContext(ctx, s.bind(q), userID).Scan(&n); err != nil {
		return false, fmt.Errorf("auth: has totp: %w", err)
	}
	return n > 0, nil
}

// BeginTOTPEnrollment generates a fresh TOTP secret, encrypts and stores it as
// UNCONFIRMED (so it is not yet a live factor), and returns the base32 secret
// and the otpauth:// URL for the QR code. Replaces any prior unconfirmed
// enrollment. Refuses when a confirmed TOTP already exists.
func (s *Store) BeginTOTPEnrollment(ctx context.Context, u User) (secret, otpauthURL string, err error) {
	if len(s.mfaKey) != 32 {
		return "", "", ErrMFAKeyMissing
	}
	confirmed, err := s.HasConfirmedTOTP(ctx, u.ID)
	if err != nil {
		return "", "", err
	}
	if confirmed {
		return "", "", ErrTOTPAlreadyEnabled
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: u.Username,
		Period:      totpPeriod,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", "", fmt.Errorf("auth: generate totp: %w", err)
	}
	enc, err := s.encryptSecret([]byte(key.Secret()))
	if err != nil {
		return "", "", err
	}
	// Replace any prior unconfirmed enrollment atomically.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("auth: totp enroll tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.bind(`DELETE FROM totp_secrets WHERE user_id = $1`), u.ID); err != nil {
		return "", "", fmt.Errorf("auth: clear totp: %w", err)
	}
	const ins = `INSERT INTO totp_secrets (user_id, secret_enc, algorithm, digits, period) VALUES ($1, $2, 'SHA1', 6, $3)`
	if _, err := tx.ExecContext(ctx, s.bind(ins), u.ID, enc, totpPeriod); err != nil {
		return "", "", fmt.Errorf("auth: store totp: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("auth: totp enroll commit: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// ConfirmTOTP proves possession of the enrolled secret and promotes it to a
// live factor. Accepts only an unconfirmed enrollment; the confirming code's
// time-step is recorded so it cannot be replayed.
func (s *Store) ConfirmTOTP(ctx context.Context, userID, code string) error {
	if len(s.mfaKey) != 32 {
		return ErrMFAKeyMissing
	}
	const q = `SELECT secret_enc, confirmed_at FROM totp_secrets WHERE user_id = $1`
	var (
		enc       string
		confirmed sql.NullString
	)
	if err := s.db.QueryRowContext(ctx, s.bind(q), userID).Scan(&enc, &confirmed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoMFA
		}
		return fmt.Errorf("auth: load totp: %w", err)
	}
	if confirmed.Valid {
		return ErrTOTPAlreadyEnabled
	}
	secret, err := s.decryptSecret(enc)
	if err != nil {
		return err
	}
	step, ok := verifyTOTPCode(string(secret), code, -1)
	if !ok {
		return ErrCodeInvalid
	}
	// Guard on confirmed_at IS NULL so a concurrent double-confirm cannot both
	// win. Distinct placeholders (the sqlite bind rewriter is positional).
	const upd = `UPDATE totp_secrets SET confirmed_at = $1, last_used_step = $2, last_used_at = $3 WHERE user_id = $4 AND confirmed_at IS NULL`
	now := nowUTC()
	res, err := s.db.ExecContext(ctx, s.bind(upd), now, step, now, userID)
	if err != nil {
		return fmt.Errorf("auth: confirm totp: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Raced with a concurrent confirm; the factor is already enabled.
		return ErrTOTPAlreadyEnabled
	}
	return nil
}

// VerifyTOTP checks a code against the user's confirmed factor at login or
// step-up. Rejects a code whose time-step was already consumed (replay guard),
// then records the accepted step. Returns false on any mismatch.
func (s *Store) VerifyTOTP(ctx context.Context, userID, code string) (bool, error) {
	if len(s.mfaKey) != 32 {
		return false, ErrMFAKeyMissing
	}
	const q = `SELECT secret_enc, last_used_step FROM totp_secrets WHERE user_id = $1 AND confirmed_at IS NOT NULL`
	var (
		enc      string
		lastStep sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx, s.bind(q), userID).Scan(&enc, &lastStep); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNoMFA
		}
		return false, fmt.Errorf("auth: load totp: %w", err)
	}
	secret, err := s.decryptSecret(enc)
	if err != nil {
		return false, err
	}
	minStep := int64(-1)
	if lastStep.Valid {
		minStep = lastStep.Int64
	}
	step, ok := verifyTOTPCode(string(secret), code, minStep)
	if !ok {
		return false, nil
	}
	// Claim the step ATOMICALLY. The guarded UPDATE advances last_used_step only
	// when no concurrent request already consumed this step (or a later one), so
	// the same code cannot be spent twice under a race (the read/verify/update
	// TOCTOU otherwise lets two requests both win). Mirrors ConsumeRecoveryCode.
	// Distinct placeholders: the sqlite bind rewriter is positional, no $N reuse.
	const upd = `UPDATE totp_secrets SET last_used_step = $1, last_used_at = $2 WHERE user_id = $3 AND (last_used_step IS NULL OR last_used_step < $4)`
	res, err := s.db.ExecContext(ctx, s.bind(upd), step, nowUTC(), userID, step)
	if err != nil {
		return false, fmt.Errorf("auth: bump totp: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Lost the race: this step was already consumed. Reject as a replay.
		return false, nil
	}
	return true, nil
}

// DisableTOTP removes the user's TOTP factor. Step-up is enforced by the caller.
func (s *Store) DisableTOTP(ctx context.Context, userID string) error {
	const q = `DELETE FROM totp_secrets WHERE user_id = $1`
	if _, err := s.db.ExecContext(ctx, s.bind(q), userID); err != nil {
		return fmt.Errorf("auth: disable totp: %w", err)
	}
	return nil
}

// verifyTOTPCode checks code against secret across the drift window, returning
// the matched time-step. It never accepts a step <= minStep, which the caller
// sets to the last consumed step so a code cannot be replayed within its own
// validity window. Constant-time compares the candidate codes.
func verifyTOTPCode(secret, code string, minStep int64) (int64, bool) {
	if code == "" {
		return 0, false
	}
	current := time.Now().UTC().Unix() / totpPeriod
	for delta := int64(-totpSkew); delta <= totpSkew; delta++ {
		step := current + delta
		if step <= minStep {
			continue
		}
		want, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0).UTC(), totp.ValidateOpts{
			Period:    totpPeriod,
			Skew:      0,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			continue
		}
		if constTimeEqualBytes([]byte(want), []byte(code)) {
			return step, true
		}
	}
	return 0, false
}
