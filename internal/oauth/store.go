// Package oauth manages OAuth2 connections: operator-configured providers and
// per-tenant connected accounts whose tokens are encrypted at rest with the
// master key. Workflows fetch a fresh access token by connection id without
// ever handling the OAuth dance or the raw refresh token.
package oauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/bright-interaction/reactor/internal/vault"
)

// secureURL allows https anywhere, or http only on loopback (local dev/tests).
func secureURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// Engine selects the SQL dialect for the engine-portable queries.
type Engine int

const (
	EngineSQLite Engine = iota
	EnginePostgres
)

// ErrNotFound is returned when a provider or connection id doesn't resolve.
var ErrNotFound = errors.New("oauth: not found")

// Provider is an operator-configured OAuth2 client app.
type Provider struct {
	ProviderID   string
	Name         string
	AuthURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string // decrypted only in memory; never serialized to the UI
	Scopes       string
	Enabled      bool
	HasSecret    bool
}

// Connection is a connected account for a tenant.
type Connection struct {
	ID         string
	TenantID   string
	ProviderID string
	Name       string
	Scopes     string
	Status     string
	ExpiresAt  time.Time
	CreatedBy  string
	CreatedAt  time.Time
}

// tokenBlob is the encrypted payload of a connection.
type tokenBlob struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// Store persists providers + connections and runs the OAuth2 flow.
type Store struct {
	db        *sql.DB
	engine    Engine
	masterKey []byte
	http      *http.Client
	now       func() time.Time

	// ProfileFor optionally returns per-provider OAuth quirks (token-exchange
	// auth style, extra authorize params) so the generic flow can talk to
	// providers that deviate from the plain spec. The daemon wires this to the
	// service catalog; nil means the default behaviour (creds in the body).
	ProfileFor func(providerID string) Profile
}

// Profile holds the optional, provider-specific knobs the OAuth flow honours.
// It keeps this package free of the catalog: the caller supplies a resolver.
type Profile struct {
	// TokenAuthStyle == "basic" sends the client id/secret as an HTTP Basic
	// header on the token request (required by Pipedrive, Notion, ...). Any
	// other value (including "") puts them in the form body, the prior default.
	TokenAuthStyle string
	// AuthParams are extra query parameters added to the authorize URL (e.g.
	// Notion's owner=user).
	AuthParams map[string]string
}

func (s *Store) profile(providerID string) Profile {
	if s.ProfileFor != nil {
		return s.ProfileFor(providerID)
	}
	return Profile{}
}

// New builds a Store. masterKey must be 32 bytes (the same key the vault uses).
func New(db *sql.DB, engine Engine, masterKey []byte) *Store {
	return &Store{
		db:        db,
		engine:    engine,
		masterKey: masterKey,
		http:      &http.Client{Timeout: 20 * time.Second},
		now:       time.Now,
	}
}

func (s *Store) bind(q string) string {
	if s.engine != EngineSQLite {
		return q
	}
	out := make([]byte, 0, len(q))
	for i := 0; i < len(q); i++ {
		if q[i] == '$' && i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9' {
			out = append(out, '?')
			i++
			for i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9' {
				i++
			}
			continue
		}
		out = append(out, q[i])
	}
	return string(out)
}

func (s *Store) nowVal() any {
	if s.engine == EngineSQLite {
		return s.now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return s.now().UTC()
}

func (s *Store) timeVal(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	if s.engine == EngineSQLite {
		return t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return t.UTC()
}

func (s *Store) boolVal(b bool) any {
	if s.engine == EngineSQLite {
		if b {
			return 1
		}
		return 0
	}
	return b
}

func parseBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case []byte:
		return len(x) == 1 && x[0] == 't'
	case string:
		return x == "t" || x == "true" || x == "1"
	}
	return false
}

func (s *Store) parseTime(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x.UTC()
	case string:
		for _, layout := range []string{"2006-01-02T15:04:05.000Z", time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, x); err == nil {
				return t.UTC()
			}
		}
	case []byte:
		return s.parseTime(string(x))
	}
	return time.Time{}
}

func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("oauth: crypto/rand failed: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// --- Provider CRUD -------------------------------------------------------

// UpsertProvider creates or updates a provider. A non-empty clientSecret is
// encrypted; an empty one leaves the stored secret untouched on update.
func (s *Store) UpsertProvider(ctx context.Context, p Provider, clientSecret string) error {
	if !secureURL(p.AuthURL) || !secureURL(p.TokenURL) {
		return errors.New("oauth: auth_url and token_url must be https (or http on localhost)")
	}
	var enc []byte
	if clientSecret != "" {
		e, err := vault.Encrypt(s.masterKey, []byte(clientSecret))
		if err != nil {
			return fmt.Errorf("oauth: encrypt client secret: %w", err)
		}
		enc = e
	}
	// Two paths so an update without a new secret keeps the old one.
	if enc != nil {
		const q = `INSERT INTO oauth_providers
			(provider_id, name, auth_url, token_url, client_id, client_secret_encrypted, scopes, enabled, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (provider_id) DO UPDATE SET
				name=excluded.name, auth_url=excluded.auth_url, token_url=excluded.token_url,
				client_id=excluded.client_id, client_secret_encrypted=excluded.client_secret_encrypted,
				scopes=excluded.scopes, enabled=excluded.enabled, updated_at=excluded.updated_at`
		_, err := s.db.ExecContext(ctx, s.bind(q),
			p.ProviderID, p.Name, p.AuthURL, p.TokenURL, p.ClientID, enc, p.Scopes, s.boolVal(p.Enabled), s.nowVal())
		return wrap("upsert provider", err)
	}
	const q = `INSERT INTO oauth_providers
		(provider_id, name, auth_url, token_url, client_id, scopes, enabled, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (provider_id) DO UPDATE SET
			name=excluded.name, auth_url=excluded.auth_url, token_url=excluded.token_url,
			client_id=excluded.client_id, scopes=excluded.scopes, enabled=excluded.enabled,
			updated_at=excluded.updated_at`
	_, err := s.db.ExecContext(ctx, s.bind(q),
		p.ProviderID, p.Name, p.AuthURL, p.TokenURL, p.ClientID, p.Scopes, s.boolVal(p.Enabled), s.nowVal())
	return wrap("upsert provider", err)
}

// GetProvider returns a provider with its client secret decrypted (for the
// flow). Returns ErrNotFound for an unknown id.
func (s *Store) GetProvider(ctx context.Context, id string) (Provider, error) {
	const q = `SELECT provider_id, name, auth_url, token_url, client_id, client_secret_encrypted, scopes, enabled
		FROM oauth_providers WHERE provider_id = $1`
	var (
		p    Provider
		enc  []byte
		enab any
	)
	err := s.db.QueryRowContext(ctx, s.bind(q), id).
		Scan(&p.ProviderID, &p.Name, &p.AuthURL, &p.TokenURL, &p.ClientID, &enc, &p.Scopes, &enab)
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	if err != nil {
		return Provider{}, wrap("get provider", err)
	}
	p.Enabled = parseBool(enab)
	p.HasSecret = len(enc) > 0
	if len(enc) > 0 {
		dec, derr := vault.Decrypt(s.masterKey, enc)
		if derr != nil {
			return Provider{}, fmt.Errorf("oauth: decrypt client secret: %w", derr)
		}
		p.ClientSecret = string(dec)
	}
	return p, nil
}

// ListProviders returns all providers WITHOUT decrypting secrets (HasSecret
// only). For the admin UI.
func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	const q = `SELECT provider_id, name, auth_url, token_url, client_id,
		(client_secret_encrypted IS NOT NULL), scopes, enabled
		FROM oauth_providers ORDER BY provider_id`
	rows, err := s.db.QueryContext(ctx, s.bind(q))
	if err != nil {
		return nil, wrap("list providers", err)
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		var hasSecret, enab any
		if err := rows.Scan(&p.ProviderID, &p.Name, &p.AuthURL, &p.TokenURL, &p.ClientID, &hasSecret, &p.Scopes, &enab); err != nil {
			return nil, err
		}
		p.HasSecret = parseBool(hasSecret)
		p.Enabled = parseBool(enab)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProvider removes a provider (and its connections via FK cascade).
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.bind(`DELETE FROM oauth_providers WHERE provider_id = $1`), id)
	return wrap("delete provider", err)
}

// --- Connection reads ----------------------------------------------------

// ListConnections returns a tenant's connections (no tokens). Pass "" for all
// (admin).
func (s *Store) ListConnections(ctx context.Context, tenantID string) ([]Connection, error) {
	q := `SELECT id, tenant_id, provider_id, name, scopes, status, expires_at, created_by, created_at
		FROM oauth_connections`
	var args []any
	if tenantID != "" {
		q += ` WHERE tenant_id = $1`
		args = append(args, tenantID)
	}
	q += ` ORDER BY provider_id, name`
	rows, err := s.db.QueryContext(ctx, s.bind(q), args...)
	if err != nil {
		return nil, wrap("list connections", err)
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, err := s.scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func (s *Store) scanConn(r rowScanner) (Connection, error) {
	var c Connection
	var expires, created any
	if err := r.Scan(&c.ID, &c.TenantID, &c.ProviderID, &c.Name, &c.Scopes, &c.Status, &expires, &c.CreatedBy, &created); err != nil {
		return Connection{}, err
	}
	c.ExpiresAt = s.parseTime(expires)
	c.CreatedAt = s.parseTime(created)
	return c, nil
}

// DeleteConnection removes a connection. tenantID guards cross-tenant deletes
// ("" = no guard, admin).
func (s *Store) DeleteConnection(ctx context.Context, id, tenantID string) error {
	q := `DELETE FROM oauth_connections WHERE id = $1`
	args := []any{id}
	if tenantID != "" {
		q += ` AND tenant_id = $2`
		args = append(args, tenantID)
	}
	_, err := s.db.ExecContext(ctx, s.bind(q), args...)
	return wrap("delete connection", err)
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("oauth: %s: %w", op, err)
}

func (s *Store) saveTokenBlob(tb tokenBlob) ([]byte, error) {
	raw, err := json.Marshal(tb)
	if err != nil {
		return nil, err
	}
	return vault.Encrypt(s.masterKey, raw)
}

func (s *Store) loadTokenBlob(enc []byte) (tokenBlob, error) {
	raw, err := vault.Decrypt(s.masterKey, enc)
	if err != nil {
		return tokenBlob{}, err
	}
	var tb tokenBlob
	return tb, json.Unmarshal(raw, &tb)
}
