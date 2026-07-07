package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// MFA-specific errors.
var (
	ErrChallengeNotFound = errors.New("auth: webauthn challenge not found or already used")
	ErrChallengeExpired  = errors.New("auth: webauthn challenge expired")
	ErrNoMFA             = errors.New("auth: user has no MFA factor enrolled")
	ErrCodeInvalid       = errors.New("auth: verification code is invalid")
)

// StepUpWindow is how long a fresh factor assertion elevates a session for
// dangerous actions ("sudo mode"). Short by design: it forces re-assertion for
// a later dangerous action rather than leaving a long standing elevation.
const StepUpWindow = 5 * time.Minute

// challengeTTL bounds how long a begun WebAuthn ceremony may be finished.
const challengeTTL = 5 * time.Minute

// SessionState carries a session's MFA assurance level alongside the User.
type SessionState struct {
	IDHash        string
	MFAPending    bool      // password accepted, second factor still owed
	ElevatedUntil time.Time // step-up "sudo" window expiry; zero if never elevated
}

// IsElevated reports whether the session is inside a live step-up window.
func (st SessionState) IsElevated() bool {
	return !st.ElevatedUntil.IsZero() && time.Now().UTC().Before(st.ElevatedUntil)
}

// WebAuthnAvailable reports whether passkeys are configured (an RP is set).
func (s *Store) WebAuthnAvailable() bool { return s.wa != nil }

// TOTPAvailable reports whether TOTP is configured (an encryption key is set).
func (s *Store) TOTPAvailable() bool { return len(s.mfaKey) == 32 }

// CreatePendingSession mints a session that is authenticated by password but
// still owes a second factor (mfa_pending=1). Until the factor clears, the
// auth middleware grants it nothing but the MFA + logout routes. Returns the
// raw cookie value.
func (s *Store) CreatePendingSession(ctx context.Context, userID, userAgent, ip string, ttl time.Duration) (string, error) {
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		return "", fmt.Errorf("auth: session rand: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(rawBytes)
	hashHex := hashToken(raw)
	expires := time.Now().UTC().Add(ttl)
	const q = `INSERT INTO sessions (id_hash, user_id, expires_at, user_agent, ip, mfa_pending) VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := s.db.ExecContext(ctx, s.bind(q), hashHex, userID, formatTime(expires), userAgent, ip, s.boolValue(true)); err != nil {
		return "", fmt.Errorf("auth: create pending session: %w", err)
	}
	return raw, nil
}

// ClearSessionMFAPending marks a session's second factor as satisfied and, in
// the same write, mints a step-up window (a successful factor assertion at
// login is itself a fresh assertion). Returns ErrNotFound when the cookie does
// not resolve to a live session.
func (s *Store) ClearSessionMFAPending(ctx context.Context, cookieValue string) error {
	hashHex := hashToken(cookieValue)
	elevated := formatTime(time.Now().UTC().Add(StepUpWindow))
	const q = `UPDATE sessions SET mfa_pending = $1, elevated_until = $2 WHERE id_hash = $3`
	res, err := s.db.ExecContext(ctx, s.bind(q), s.boolValue(false), elevated, hashHex)
	if err != nil {
		return fmt.Errorf("auth: clear mfa pending: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkSessionElevated opens a fresh step-up window on a session. Called after a
// standalone factor assertion (passkey or TOTP) authorizes a dangerous action.
func (s *Store) MarkSessionElevated(ctx context.Context, cookieValue string, window time.Duration) error {
	hashHex := hashToken(cookieValue)
	until := formatTime(time.Now().UTC().Add(window))
	const q = `UPDATE sessions SET elevated_until = $1 WHERE id_hash = $2`
	res, err := s.db.ExecContext(ctx, s.bind(q), until, hashHex)
	if err != nil {
		return fmt.Errorf("auth: mark elevated: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// HasMFA reports whether the user has at least one live factor: a registered
// passkey or a confirmed TOTP enrollment. Unconfirmed TOTP (enrollment begun
// but never proven) does not count.
func (s *Store) HasMFA(ctx context.Context, userID string) (bool, error) {
	// Distinct placeholders per position: the sqlite bind rewriter is a naive
	// positional $N -> ? converter, so a reused $1 needs the arg passed twice.
	const q = `SELECT
		(SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = $1) +
		(SELECT COUNT(*) FROM totp_secrets WHERE user_id = $2 AND confirmed_at IS NOT NULL)`
	var n int
	if err := s.db.QueryRowContext(ctx, s.bind(q), userID, userID).Scan(&n); err != nil {
		return false, fmt.Errorf("auth: has mfa: %w", err)
	}
	return n > 0, nil
}

// PasskeyInfo is a UI-facing summary of one registered passkey (no key material).
type PasskeyInfo struct {
	ID           string
	Name         string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	CloneWarning bool
	BackedUp     bool
}

// MFAFactors is the /security page summary of a user's enrolled factors.
type MFAFactors struct {
	Passkeys     []PasskeyInfo
	TOTPEnabled  bool
	RecoveryLeft int
}

// ListMFAFactors gathers a user's factor summary for the dashboard.
func (s *Store) ListMFAFactors(ctx context.Context, userID string) (MFAFactors, error) {
	var out MFAFactors
	keys, err := s.listStoredCredentials(ctx, userID)
	if err != nil {
		return out, err
	}
	for _, k := range keys {
		out.Passkeys = append(out.Passkeys, PasskeyInfo{
			ID:           k.RowID,
			Name:         k.Name,
			CreatedAt:    k.CreatedAt,
			LastUsedAt:   k.LastUsedAt,
			CloneWarning: k.Cred.Authenticator.CloneWarning,
			BackedUp:     k.Cred.Flags.BackupState,
		})
	}
	totp, err := s.HasConfirmedTOTP(ctx, userID)
	if err != nil {
		return out, err
	}
	out.TOTPEnabled = totp
	left, err := s.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return out, err
	}
	out.RecoveryLeft = left
	return out, nil
}

// putChallenge stores a begun ceremony's SessionData under a fresh opaque id,
// pinned to the user and purpose, expiring after challengeTTL. The id is handed
// to the browser and echoed back at finish.
func (s *Store) putChallenge(ctx context.Context, userID, purpose string, sd *webauthn.SessionData) (string, error) {
	id, err := newID("wch_")
	if err != nil {
		return "", err
	}
	blob, err := json.Marshal(sd)
	if err != nil {
		return "", fmt.Errorf("auth: marshal challenge: %w", err)
	}
	expires := formatTime(time.Now().UTC().Add(challengeTTL))
	const q = `INSERT INTO webauthn_challenges (id, user_id, purpose, session_data, expires_at) VALUES ($1, $2, $3, $4, $5)`
	if _, err := s.db.ExecContext(ctx, s.bind(q), id, userID, purpose, string(blob), expires); err != nil {
		return "", fmt.Errorf("auth: put challenge: %w", err)
	}
	return id, nil
}

// takeChallenge consumes a challenge exactly once. It reads then deletes the
// row inside a transaction, using the DELETE's rows-affected to guarantee a
// concurrent double-submit cannot both succeed. Returns ErrChallengeNotFound
// when missing/already used, ErrChallengeExpired when past its TTL (still
// consuming it so it cannot be retried), and enforces the expected purpose so
// a challenge minted for one flow cannot satisfy another.
func (s *Store) takeChallenge(ctx context.Context, id, purpose string) (*webauthn.SessionData, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("auth: take challenge tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		userID     string
		blob       string
		expiresStr string
	)
	const sel = `SELECT user_id, session_data, expires_at FROM webauthn_challenges WHERE id = $1 AND purpose = $2`
	if err := tx.QueryRowContext(ctx, s.bind(sel), id, purpose).Scan(&userID, &blob, &expiresStr); err != nil {
		return nil, "", ErrChallengeNotFound
	}
	const del = `DELETE FROM webauthn_challenges WHERE id = $1 AND purpose = $2`
	res, err := tx.ExecContext(ctx, s.bind(del), id, purpose)
	if err != nil {
		return nil, "", fmt.Errorf("auth: take challenge delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Lost a race with a concurrent consumer.
		return nil, "", ErrChallengeNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("auth: take challenge commit: %w", err)
	}
	// Fail closed: an unparseable expiry is treated as expired rather than
	// silently accepted (the go-webauthn library timeout is not enabled here, so
	// this is the only TTL gate).
	if expires, perr := parseTime(expiresStr); perr != nil || time.Now().UTC().After(expires) {
		return nil, "", ErrChallengeExpired
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal([]byte(blob), &sd); err != nil {
		return nil, "", fmt.Errorf("auth: unmarshal challenge: %w", err)
	}
	return &sd, userID, nil
}

// SweepExpiredChallenges drops stale ceremony rows. Called periodically.
func (s *Store) SweepExpiredChallenges(ctx context.Context) (int64, error) {
	const q = `DELETE FROM webauthn_challenges WHERE expires_at < $1`
	res, err := s.db.ExecContext(ctx, s.bind(q), nowUTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
