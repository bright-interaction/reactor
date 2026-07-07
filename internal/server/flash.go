package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// flashStore is a tiny in-memory single-use store. Used by the webhook-
// trigger creation handler to surface the freshly-minted HMAC secret on
// the redirect-target page exactly once, without putting it in the URL
// query string (where it would land in access logs, browser history,
// and Referer headers sent on subsequent same-origin sub-resource loads).
//
// Each entry is keyed by a random 32-char hex token. The token lives in
// a flash cookie (HttpOnly, SameSite=Strict, short MaxAge). The
// redirect-target page reads the token from the cookie, looks up the
// payload, deletes the entry, and renders the secret in the response
// body. Subsequent loads of the same URL see no payload.
//
// TTL caps stale entries at 5 minutes so an operator who navigates
// away never sees the secret bleed into a later session.
type flashStore struct {
	mu  sync.Mutex
	m   map[string]flashEntry
	ttl time.Duration
}

type flashEntry struct {
	payload map[string]string
	expires time.Time
}

const flashCookieName = "reactor_flash"

func newFlashStore() *flashStore {
	return &flashStore{
		m:   map[string]flashEntry{},
		ttl: 5 * time.Minute,
	}
}

// put stashes payload under a fresh random token and writes a cookie
// carrying that token. The next request from the same browser to the
// redirect-target page calls take(r) to consume it.
func (s *flashStore) put(w http.ResponseWriter, r *http.Request, payload map[string]string) error {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	s.mu.Lock()
	s.m[token] = flashEntry{
		payload: payload,
		expires: time.Now().Add(s.ttl),
	}
	s.gcLocked()
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.ttl.Seconds()),
	})
	return nil
}

// take reads the flash cookie, removes the corresponding entry, and
// clears the cookie. Returns nil when no flash is set or the token is
// stale.
func (s *flashStore) take(w http.ResponseWriter, r *http.Request) map[string]string {
	c, err := r.Cookie(flashCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	// Always clear the cookie so a second load doesn't redo the lookup.
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.m[c.Value]
	if !ok {
		return nil
	}
	delete(s.m, c.Value)
	if time.Now().After(entry.expires) {
		return nil
	}
	return entry.payload
}

// gcLocked drops expired entries. Cheap O(n) sweep; the store rarely
// holds more than a handful of entries (operator presses Create
// Webhook, picks up the secret on the next page load).
func (s *flashStore) gcLocked() {
	now := time.Now()
	for k, e := range s.m {
		if now.After(e.expires) {
			delete(s.m, k)
		}
	}
}
