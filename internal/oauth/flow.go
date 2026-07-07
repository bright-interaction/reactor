package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// stateTTL bounds how long an authorize->callback round trip may take.
const stateTTL = 10 * time.Minute

// StartAuth begins the authorization-code flow: it records CSRF + PKCE state
// and returns the provider authorize URL to redirect the user to. The provider
// must be enabled and configured with a client id.
func (s *Store) StartAuth(ctx context.Context, tenantID, providerID, name, redirectURI, createdBy string) (string, error) {
	p, err := s.GetProvider(ctx, providerID)
	if err != nil {
		return "", err
	}
	if !p.Enabled || p.ClientID == "" {
		return "", errors.New("oauth: provider is not configured (needs client id + enabled)")
	}
	if name == "" {
		return "", errors.New("oauth: a connection name is required")
	}
	s.gcStates(ctx)

	state := newID("st_")
	verifier := randString(48)
	challenge := pkceChallenge(verifier)

	const q = `INSERT INTO oauth_states (state, tenant_id, provider_id, name, redirect_uri, code_verifier, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	if _, err := s.db.ExecContext(ctx, s.bind(q),
		state, tenantID, providerID, name, redirectURI, verifier, createdBy, s.nowVal()); err != nil {
		return "", wrap("save state", err)
	}

	scopes := p.Scopes
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", p.ClientID)
	v.Set("redirect_uri", redirectURI)
	if scopes != "" {
		v.Set("scope", scopes)
	}
	v.Set("state", state)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	// Ask for a refresh token on providers that gate it (Google).
	v.Set("access_type", "offline")
	v.Set("prompt", "consent")
	// Provider-specific authorize params (e.g. Notion's owner=user).
	for k, val := range s.profile(providerID).AuthParams {
		v.Set(k, val)
	}
	sep := "?"
	if strings.Contains(p.AuthURL, "?") {
		sep = "&"
	}
	return p.AuthURL + sep + v.Encode(), nil
}

// CompleteAuth handles the provider redirect back: it validates + consumes the
// state, exchanges the code for tokens, and stores an encrypted connection.
func (s *Store) CompleteAuth(ctx context.Context, state, code string) (Connection, error) {
	if state == "" || code == "" {
		return Connection{}, errors.New("oauth: missing state or code")
	}
	var (
		tenantID, providerID, name, redirectURI, verifier, createdBy string
		createdAny                                                   any
	)
	const sel = `SELECT tenant_id, provider_id, name, redirect_uri, code_verifier, created_by, created_at
		FROM oauth_states WHERE state = $1`
	err := s.db.QueryRowContext(ctx, s.bind(sel), state).
		Scan(&tenantID, &providerID, &name, &redirectURI, &verifier, &createdBy, &createdAny)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, errors.New("oauth: unknown or expired state")
	}
	if err != nil {
		return Connection{}, wrap("load state", err)
	}
	// Single-use: consume the state regardless of outcome.
	_, _ = s.db.ExecContext(ctx, s.bind(`DELETE FROM oauth_states WHERE state = $1`), state)
	if created := s.parseTime(createdAny); created.IsZero() || s.now().UTC().Sub(created) > stateTTL {
		return Connection{}, errors.New("oauth: state expired, restart the connection")
	}

	p, err := s.GetProvider(ctx, providerID)
	if err != nil {
		return Connection{}, err
	}
	tok, err := s.exchange(ctx, p, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		return Connection{}, err
	}
	return s.upsertConnection(ctx, tenantID, providerID, name, createdBy, tok)
}

// Token returns a valid access token for a connection, refreshing it first if
// it is expired (or about to be) and a refresh token is available. tenantID, if
// non-empty, guards against cross-tenant access. This is the workflow-facing
// accessor.
func (s *Store) Token(ctx context.Context, connectionID, tenantID string) (string, error) {
	q := `SELECT id, tenant_id, provider_id, token_encrypted FROM oauth_connections WHERE id = $1`
	args := []any{connectionID}
	if tenantID != "" {
		q += ` AND tenant_id = $2`
		args = append(args, tenantID)
	}
	var id, ten, providerID string
	var enc []byte
	err := s.db.QueryRowContext(ctx, s.bind(q), args...).Scan(&id, &ten, &providerID, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", wrap("load connection", err)
	}
	tb, err := s.loadTokenBlob(enc)
	if err != nil {
		return "", fmt.Errorf("oauth: decrypt token: %w", err)
	}
	// Fresh enough? (60s skew guard.)
	if tb.Expiry.IsZero() || s.now().UTC().Before(tb.Expiry.Add(-60*time.Second)) {
		return tb.AccessToken, nil
	}
	if tb.RefreshToken == "" {
		return tb.AccessToken, nil // no refresh available; return what we have
	}
	p, err := s.GetProvider(ctx, providerID)
	if err != nil {
		return "", err
	}
	refreshed, err := s.exchange(ctx, p, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tb.RefreshToken},
	})
	if err != nil {
		_ = s.markStatus(ctx, id, "error")
		return "", err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tb.RefreshToken // providers often omit it on refresh
	}
	if _, err := s.upsertConnection(ctx, ten, providerID, connNameByID(ctx, s, id), "", refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

// exchange POSTs to the provider token endpoint and parses the token response.
func (s *Store) exchange(ctx context.Context, p Provider, form url.Values) (tokenBlob, error) {
	basic := s.profile(p.ProviderID).TokenAuthStyle == "basic"
	// Client id is always safe in the body (identifies the app). The secret
	// goes either in the body (default) or an HTTP Basic header (when the
	// provider requires it, e.g. Pipedrive, Notion).
	form.Set("client_id", p.ClientID)
	if p.ClientSecret != "" && !basic {
		form.Set("client_secret", p.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenBlob{}, wrap("token request", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic && p.ClientSecret != "" {
		req.SetBasicAuth(p.ClientID, p.ClientSecret)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return tokenBlob{}, wrap("token exchange", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return tokenBlob{}, fmt.Errorf("oauth: token endpoint %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.AccessToken == "" {
		// Some providers reply form-encoded; fall back.
		if vals, perr := url.ParseQuery(string(body)); perr == nil && vals.Get("access_token") != "" {
			raw.AccessToken = vals.Get("access_token")
			raw.RefreshToken = vals.Get("refresh_token")
			raw.TokenType = vals.Get("token_type")
			raw.Scope = vals.Get("scope")
		} else {
			return tokenBlob{}, fmt.Errorf("oauth: token response had no access_token")
		}
	}
	tb := tokenBlob{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		TokenType:    raw.TokenType,
		Scope:        raw.Scope,
	}
	if raw.ExpiresIn > 0 {
		tb.Expiry = s.now().UTC().Add(time.Duration(raw.ExpiresIn) * time.Second)
	}
	return tb, nil
}

func (s *Store) upsertConnection(ctx context.Context, tenantID, providerID, name, createdBy string, tok tokenBlob) (Connection, error) {
	enc, err := s.saveTokenBlob(tok)
	if err != nil {
		return Connection{}, fmt.Errorf("oauth: encrypt token: %w", err)
	}
	id := newID("conn_")
	const q = `INSERT INTO oauth_connections
		(id, tenant_id, provider_id, name, token_encrypted, expires_at, scopes, status, created_by, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'connected',$8,$9)
		ON CONFLICT (tenant_id, provider_id, name) DO UPDATE SET
			token_encrypted=excluded.token_encrypted, expires_at=excluded.expires_at,
			scopes=excluded.scopes, status='connected', updated_at=excluded.updated_at`
	if _, err := s.db.ExecContext(ctx, s.bind(q),
		id, tenantID, providerID, name, enc, s.timeVal(tok.Expiry), tok.Scope, createdBy, s.nowVal()); err != nil {
		return Connection{}, wrap("save connection", err)
	}
	return Connection{ID: id, TenantID: tenantID, ProviderID: providerID, Name: name,
		Scopes: tok.Scope, Status: "connected", ExpiresAt: tok.Expiry, CreatedBy: createdBy}, nil
}

func (s *Store) markStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE oauth_connections SET status=$1, updated_at=$2 WHERE id=$3`),
		status, s.nowVal(), id)
	return err
}

func (s *Store) gcStates(ctx context.Context) {
	cutoff := s.timeVal(s.now().UTC().Add(-stateTTL))
	_, _ = s.db.ExecContext(ctx, s.bind(`DELETE FROM oauth_states WHERE created_at < $1`), cutoff)
}

func connNameByID(ctx context.Context, s *Store, id string) string {
	var name string
	_ = s.db.QueryRowContext(ctx, s.bind(`SELECT name FROM oauth_connections WHERE id = $1`), id).Scan(&name)
	return name
}

func randString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is catastrophic; never emit a predictable
		// OAuth state / PKCE verifier from a zeroed buffer.
		panic("oauth: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
