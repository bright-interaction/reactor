package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Ceremony purposes (also the CHECK values on webauthn_challenges.purpose).
// Exported so the server layer can name the assertion purpose type-safely.
const (
	PurposeRegister = "register"
	PurposeLogin    = "login"
	PurposeStepUp   = "stepup"
)

// ErrCloneWarning is returned when an assertion's signature counter regressed,
// which per the WebAuthn spec signals a possibly cloned authenticator. The
// login/step-up is refused and the credential is flagged for the operator.
var ErrCloneWarning = errors.New("auth: authenticator signature counter regressed (possible clone); assertion refused")

// ErrPasskeyExists is returned when finishing a registration whose authenticator
// is already enrolled for this account.
var ErrPasskeyExists = errors.New("auth: this passkey is already registered")

// storedCredential is one webauthn_credentials row with the JSON blob decoded.
type storedCredential struct {
	RowID        string
	CredentialID string // base64url(raw credential id)
	Cred         webauthn.Credential
	Name         string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}

// waUser adapts an auth.User + its stored credentials to the webauthn.User
// interface the library needs for both ceremonies. WebAuthnID is the stable DB
// user id (an opaque handle, well under the 64-byte limit).
type waUser struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.id }
func (u *waUser) WebAuthnName() string                       { return u.name }
func (u *waUser) WebAuthnDisplayName() string                { return u.name }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (u *waUser) descriptors() []protocol.CredentialDescriptor {
	out := make([]protocol.CredentialDescriptor, 0, len(u.creds))
	for _, c := range u.creds {
		out = append(out, c.Descriptor())
	}
	return out
}

// buildWAUser loads a user's stored credentials into a webauthn.User.
func (s *Store) buildWAUser(ctx context.Context, u User) (*waUser, error) {
	stored, err := s.listStoredCredentials(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	creds := make([]webauthn.Credential, len(stored))
	for i, sc := range stored {
		creds[i] = sc.Cred
	}
	name := u.Username
	if name == "" {
		name = u.ID
	}
	return &waUser{id: []byte(u.ID), name: name, creds: creds}, nil
}

// listStoredCredentials returns a user's passkeys with the credential blob decoded.
func (s *Store) listStoredCredentials(ctx context.Context, userID string) ([]storedCredential, error) {
	const q = `SELECT id, credential_id, credential, name, created_at, last_used_at
		FROM webauthn_credentials WHERE user_id = $1 ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, s.bind(q), userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list credentials: %w", err)
	}
	defer rows.Close()
	var out []storedCredential
	for rows.Next() {
		var (
			sc       storedCredential
			blob     string
			created  sql.NullString
			lastUsed sql.NullString
		)
		if err := rows.Scan(&sc.RowID, &sc.CredentialID, &blob, &sc.Name, &created, &lastUsed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(blob), &sc.Cred); err != nil {
			return nil, fmt.Errorf("auth: decode credential %s: %w", sc.RowID, err)
		}
		if created.Valid {
			if t, perr := parseTime(created.String); perr == nil {
				sc.CreatedAt = t
			}
		}
		if lastUsed.Valid {
			if t, perr := parseTime(lastUsed.String); perr == nil {
				sc.LastUsedAt = &t
			}
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// BeginWebAuthnRegistration starts a passkey enrollment: it excludes the user's
// existing authenticators (so one cannot be double-registered), stores the
// ceremony SessionData, and returns the JSON creation options for the browser
// plus the opaque challenge id to echo back at finish.
func (s *Store) BeginWebAuthnRegistration(ctx context.Context, u User) (optionsJSON []byte, challengeID string, err error) {
	if s.wa == nil {
		return nil, "", ErrWebAuthnMissing
	}
	wu, err := s.buildWAUser(ctx, u)
	if err != nil {
		return nil, "", err
	}
	sel := protocol.AuthenticatorSelection{
		ResidentKey:      protocol.ResidentKeyRequirementPreferred,
		UserVerification: protocol.VerificationPreferred,
	}
	opts, sd, err := s.wa.BeginRegistration(wu,
		webauthn.WithExclusions(wu.descriptors()),
		webauthn.WithAuthenticatorSelection(sel),
	)
	if err != nil {
		return nil, "", fmt.Errorf("auth: begin registration: %w", err)
	}
	id, err := s.putChallenge(ctx, u.ID, PurposeRegister, sd)
	if err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(opts)
	if err != nil {
		return nil, "", fmt.Errorf("auth: marshal creation options: %w", err)
	}
	return body, id, nil
}

// FinishWebAuthnRegistration consumes the challenge, verifies the attestation
// response, and stores the new credential under the operator's chosen name.
func (s *Store) FinishWebAuthnRegistration(ctx context.Context, u User, challengeID string, responseBody []byte, name string) error {
	if s.wa == nil {
		return ErrWebAuthnMissing
	}
	sd, chUser, err := s.takeChallenge(ctx, challengeID, PurposeRegister)
	if err != nil {
		return err
	}
	if chUser != u.ID {
		return ErrChallengeNotFound
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(responseBody)
	if err != nil {
		return fmt.Errorf("auth: parse registration response: %w", err)
	}
	wu, err := s.buildWAUser(ctx, u)
	if err != nil {
		return err
	}
	cred, err := s.wa.CreateCredential(wu, *sd, parsed)
	if err != nil {
		return fmt.Errorf("auth: verify registration: %w", err)
	}
	return s.insertCredential(ctx, u.ID, cred, name)
}

func (s *Store) insertCredential(ctx context.Context, userID string, cred *webauthn.Credential, name string) error {
	if name == "" {
		name = "Passkey"
	}
	id, err := newID("wac_")
	if err != nil {
		return err
	}
	blob, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("auth: marshal credential: %w", err)
	}
	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	const q = `INSERT INTO webauthn_credentials (id, user_id, credential_id, credential, name) VALUES ($1, $2, $3, $4, $5)`
	if _, err := s.db.ExecContext(ctx, s.bind(q), id, userID, credID, string(blob), name); err != nil {
		if isUniqueErr(err) {
			return ErrPasskeyExists
		}
		return fmt.Errorf("auth: insert credential: %w", err)
	}
	return nil
}

// BeginWebAuthnAssertion starts a login or step-up assertion for a known user.
// purpose must be PurposeLogin or PurposeStepUp; it pins the challenge so a
// login assertion cannot be replayed to satisfy a step-up or vice versa.
func (s *Store) BeginWebAuthnAssertion(ctx context.Context, u User, purpose string) (optionsJSON []byte, challengeID string, err error) {
	if s.wa == nil {
		return nil, "", ErrWebAuthnMissing
	}
	if purpose != PurposeLogin && purpose != PurposeStepUp {
		return nil, "", fmt.Errorf("auth: invalid assertion purpose %q", purpose)
	}
	wu, err := s.buildWAUser(ctx, u)
	if err != nil {
		return nil, "", err
	}
	if len(wu.creds) == 0 {
		return nil, "", ErrNoMFA
	}
	opts, sd, err := s.wa.BeginLogin(wu, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return nil, "", fmt.Errorf("auth: begin assertion: %w", err)
	}
	id, err := s.putChallenge(ctx, u.ID, purpose, sd)
	if err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(opts)
	if err != nil {
		return nil, "", fmt.Errorf("auth: marshal assertion options: %w", err)
	}
	return body, id, nil
}

// FinishWebAuthnAssertion consumes the challenge, validates the assertion, and
// on success persists the bumped signature counter. It refuses (ErrCloneWarning)
// when the counter regressed, flagging the credential for the operator.
func (s *Store) FinishWebAuthnAssertion(ctx context.Context, u User, purpose, challengeID string, responseBody []byte) error {
	if s.wa == nil {
		return ErrWebAuthnMissing
	}
	if purpose != PurposeLogin && purpose != PurposeStepUp {
		return fmt.Errorf("auth: invalid assertion purpose %q", purpose)
	}
	sd, chUser, err := s.takeChallenge(ctx, challengeID, purpose)
	if err != nil {
		return err
	}
	if chUser != u.ID {
		return ErrChallengeNotFound
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(responseBody)
	if err != nil {
		return fmt.Errorf("auth: parse assertion response: %w", err)
	}
	wu, err := s.buildWAUser(ctx, u)
	if err != nil {
		return err
	}
	cred, err := s.wa.ValidateLogin(wu, *sd, parsed)
	if err != nil {
		return fmt.Errorf("auth: validate assertion: %w", err)
	}
	// Persist the updated credential (bumped counter / clone flag) regardless,
	// so the counter stays monotonic and the warning is visible in the UI.
	if uerr := s.updateCredentialAfterUse(ctx, cred); uerr != nil {
		return uerr
	}
	if cred.Authenticator.CloneWarning {
		return ErrCloneWarning
	}
	return nil
}

// updateCredentialAfterUse rewrites the stored blob (new sign count, flags) and
// bumps last_used_at, matched by the stable credential id.
func (s *Store) updateCredentialAfterUse(ctx context.Context, cred *webauthn.Credential) error {
	blob, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("auth: marshal credential: %w", err)
	}
	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	const q = `UPDATE webauthn_credentials SET credential = $1, last_used_at = $2 WHERE credential_id = $3`
	if _, err := s.db.ExecContext(ctx, s.bind(q), string(blob), nowUTC(), credID); err != nil {
		return fmt.Errorf("auth: update credential: %w", err)
	}
	return nil
}

// DeleteWebAuthnCredential removes one of the user's passkeys (scoped to the
// user so an id cannot delete another account's credential).
func (s *Store) DeleteWebAuthnCredential(ctx context.Context, userID, rowID string) error {
	const q = `DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2`
	res, err := s.db.ExecContext(ctx, s.bind(q), rowID, userID)
	if err != nil {
		return fmt.Errorf("auth: delete credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RenameWebAuthnCredential relabels one of the user's passkeys.
func (s *Store) RenameWebAuthnCredential(ctx context.Context, userID, rowID, name string) error {
	if name == "" {
		name = "Passkey"
	}
	const q = `UPDATE webauthn_credentials SET name = $1 WHERE id = $2 AND user_id = $3`
	res, err := s.db.ExecContext(ctx, s.bind(q), name, rowID, userID)
	if err != nil {
		return fmt.Errorf("auth: rename credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
