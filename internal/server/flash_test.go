package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFlashRoundTripDeletesAfterTake(t *testing.T) {
	t.Parallel()
	s := newFlashStore()

	// put writes a cookie + stashes the payload.
	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := s.put(w1, r1, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	resp1 := w1.Result()
	cookies := resp1.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie count = %d, want 1", len(cookies))
	}
	c := cookies[0]
	if c.HttpOnly != true || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("flash cookie missing security attrs: %+v", c)
	}

	// take with the cookie returns the payload + clears the cookie.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/dest", nil)
	r2.AddCookie(c)
	got := s.take(w2, r2)
	if got["k"] != "v" {
		t.Fatalf("take payload = %+v, want {k:v}", got)
	}
	resp2 := w2.Result()
	clearCookies := resp2.Cookies()
	if len(clearCookies) != 1 || clearCookies[0].MaxAge != -1 {
		t.Fatalf("take must clear flash cookie via MaxAge=-1; got %+v", clearCookies)
	}

	// Second take returns nothing (single-use).
	w3 := httptest.NewRecorder()
	r3 := httptest.NewRequest(http.MethodGet, "/dest", nil)
	r3.AddCookie(c)
	if again := s.take(w3, r3); again != nil {
		t.Fatalf("second take = %+v, want nil (single-use)", again)
	}
}

func TestFlashTakeWithoutCookieReturnsNil(t *testing.T) {
	t.Parallel()
	s := newFlashStore()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := s.take(w, r); got != nil {
		t.Fatalf("take without cookie = %+v, want nil", got)
	}
}

func TestFlashExpiredEntryReturnsNil(t *testing.T) {
	t.Parallel()
	s := newFlashStore()
	s.ttl = -time.Second // entries are dead-on-arrival
	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := s.put(w1, r1, map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	c := w1.Result().Cookies()[0]

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.AddCookie(c)
	if got := s.take(w2, r2); got != nil {
		t.Fatalf("expired take = %+v, want nil", got)
	}
}
