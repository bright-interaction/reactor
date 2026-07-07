package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// recoveryCodeCount is how many one-time codes a regeneration mints.
const recoveryCodeCount = 10

// GenerateRecoveryCodes replaces the user's recovery codes with a fresh batch of
// high-entropy one-time codes, returns the plaintext (to show exactly once), and
// stores only their sha256 hashes. Regenerating invalidates every prior code.
func (s *Store) GenerateRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	codes := make([]string, 0, recoveryCodeCount)
	hashes := make([]string, 0, recoveryCodeCount)
	for i := 0; i < recoveryCodeCount; i++ {
		display, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, display)
		hashes = append(hashes, hashToken(normalizeRecoveryCode(display)))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: recovery tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.bind(`DELETE FROM mfa_recovery_codes WHERE user_id = $1`), userID); err != nil {
		return nil, fmt.Errorf("auth: clear recovery codes: %w", err)
	}
	const ins = `INSERT INTO mfa_recovery_codes (id, user_id, code_hash) VALUES ($1, $2, $3)`
	for _, h := range hashes {
		id, err := newID("rec_")
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, s.bind(ins), id, userID, h); err != nil {
			return nil, fmt.Errorf("auth: insert recovery code: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("auth: recovery commit: %w", err)
	}
	return codes, nil
}

// ConsumeRecoveryCode spends one unused recovery code for the user. The single
// UPDATE guarded by used_at IS NULL makes consumption atomic and exactly-once
// even under concurrent submits. Returns true when a code was spent.
func (s *Store) ConsumeRecoveryCode(ctx context.Context, userID, code string) (bool, error) {
	norm := normalizeRecoveryCode(code)
	if norm == "" {
		return false, nil
	}
	h := hashToken(norm)
	const q = `UPDATE mfa_recovery_codes SET used_at = $1 WHERE user_id = $2 AND code_hash = $3 AND used_at IS NULL`
	res, err := s.db.ExecContext(ctx, s.bind(q), nowUTC(), userID, h)
	if err != nil {
		return false, fmt.Errorf("auth: consume recovery code: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CountUnusedRecoveryCodes returns how many one-time codes remain unspent.
func (s *Store) CountUnusedRecoveryCodes(ctx context.Context, userID string) (int, error) {
	const q = `SELECT COUNT(*) FROM mfa_recovery_codes WHERE user_id = $1 AND used_at IS NULL`
	var n int
	if err := s.db.QueryRowContext(ctx, s.bind(q), userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: count recovery codes: %w", err)
	}
	return n, nil
}

// newRecoveryCode returns a display-formatted code (~80 bits of entropy) as
// four dash-separated base32 groups, e.g. "3f7q-9xk2-p4mz-a8dw".
func newRecoveryCode() (string, error) {
	buf := make([]byte, 10) // 80 bits
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: recovery rand: %w", err)
	}
	raw := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)) // 16 chars
	return raw[0:4] + "-" + raw[4:8] + "-" + raw[8:12] + "-" + raw[12:16], nil
}

// normalizeRecoveryCode strips formatting so a code entered with or without
// dashes/case hashes to the same value.
func normalizeRecoveryCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
