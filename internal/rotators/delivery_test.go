package rotators

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/reactor/internal/credentials"
	"github.com/bright-interaction/reactor/internal/vault"
)

func newVaultWithSecret(t *testing.T, id string, value []byte) *vault.Store {
	t.Helper()
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = 0xAA
	}
	store, err := vault.NewStore(vault.NewMemoryBackend(), masterKey)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	if err := store.Put(context.Background(), id, value); err != nil {
		t.Fatalf("vault put: %v", err)
	}
	return store
}

func TestDeliverWebhookHappyPath(t *testing.T) {
	t.Parallel()

	const sharedSecret = "hmac-secret-shhh"
	v := newVaultWithSecret(t, "cred_hmac", []byte(sharedSecret))

	var seenBody []byte
	var seenSig string
	var seenKeyName string
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		seenBody = append([]byte(nil), body...)
		seenSig = r.Header.Get("X-Reactor-Signature")
		seenKeyName = r.Header.Get("X-Reactor-Key-Name")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := credentials.Target{
		Kind:     "webhook",
		URL:      srv.URL,
		SecretID: "cred_hmac",
		KeyName:  "FOO_TOKEN",
	}
	res := Deliver(context.Background(), target, "old-val", "new-val", v)
	if !res.Success {
		t.Fatalf("delivery failed: status=%d err=%s", res.Status, res.Error)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	// Verify the signature.
	const prefix = "sha256="
	if !strings.HasPrefix(seenSig, prefix) {
		t.Fatalf("missing sha256= prefix on sig %q", seenSig)
	}
	gotMAC, err := hex.DecodeString(strings.TrimPrefix(seenSig, prefix))
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(sharedSecret))
	mac.Write(seenBody)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		t.Fatal("HMAC mismatch")
	}
	// Verify the body shape.
	var p DeliveryPayload
	if err := json.Unmarshal(seenBody, &p); err != nil {
		t.Fatalf("bad payload json: %v", err)
	}
	if p.Key != "FOO_TOKEN" || p.Value != "new-val" || p.Previous != "old-val" {
		t.Fatalf("payload mismatch: %+v", p)
	}
	if seenKeyName != "FOO_TOKEN" {
		t.Fatalf("key-name header = %q, want FOO_TOKEN", seenKeyName)
	}
}

func TestDeliverRejectsUnknownKind(t *testing.T) {
	t.Parallel()
	v := newVaultWithSecret(t, "cred_hmac", []byte("x"))
	res := Deliver(context.Background(), credentials.Target{Kind: "smoke-signal"}, "", "", v)
	if res.Success {
		t.Fatal("unknown kind should fail")
	}
	if !strings.Contains(res.Error, "unsupported target kind") {
		t.Fatalf("unexpected error: %s", res.Error)
	}
}

func TestDeliverRejectsMissingFields(t *testing.T) {
	t.Parallel()
	v := newVaultWithSecret(t, "cred_hmac", []byte("x"))
	res := Deliver(context.Background(), credentials.Target{Kind: "webhook"}, "", "", v)
	if res.Success {
		t.Fatal("missing fields should fail")
	}
}

// TestDeliverReloadEndpointDualPhase verifies the open/close grace
// flow: receiver sees two POSTs in order, first carries previous, the
// second clears it. GraceSeconds is set tiny so the test runs fast.
func TestDeliverReloadEndpointDualPhase(t *testing.T) {
	t.Parallel()
	const sharedSecret = "reload-shhh"
	v := newVaultWithSecret(t, "cred_hmac", []byte(sharedSecret))

	type call struct {
		Body  []byte
		Phase string
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, call{
			Body:  append([]byte(nil), body...),
			Phase: r.Header.Get("X-Reactor-Phase"),
		})
		// Verify HMAC.
		got := strings.TrimPrefix(r.Header.Get("X-Reactor-Signature"), "sha256=")
		mac := hmac.New(sha256.New, []byte(sharedSecret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		if got != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	target := credentials.Target{
		Kind:         "reload_endpoint",
		URL:          srv.URL,
		SecretID:     "cred_hmac",
		KeyName:      "APP_TOKEN",
		GraceSeconds: 1, // 1s grace keeps the test fast
	}
	res := Deliver(context.Background(), target, "old-token", "new-token", v)
	if !res.Success {
		t.Fatalf("delivery failed: status=%d err=%s", res.Status, res.Error)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 (open+close)", len(calls))
	}
	if calls[0].Phase != "open" || calls[1].Phase != "close" {
		t.Fatalf("phase order = [%s, %s], want [open, close]", calls[0].Phase, calls[1].Phase)
	}

	// Open phase carries both new + previous.
	var openP DeliveryPayload
	_ = json.Unmarshal(calls[0].Body, &openP)
	if openP.Value != "new-token" || openP.Previous != "old-token" || openP.Phase != "open" {
		t.Fatalf("open payload mismatch: %+v", openP)
	}

	// Close phase carries new only; previous cleared so receivers drop
	// dual-validity acceptance.
	var closeP DeliveryPayload
	_ = json.Unmarshal(calls[1].Body, &closeP)
	if closeP.Value != "new-token" || closeP.Previous != "" || closeP.Phase != "close" {
		t.Fatalf("close payload mismatch: %+v", closeP)
	}
}

// TestDeliverReloadEndpointFailsClosedOnOpen5xx ensures phase=close is
// not attempted when phase=open returns 5xx.
func TestDeliverReloadEndpointFailsClosedOnOpen5xx(t *testing.T) {
	t.Parallel()
	v := newVaultWithSecret(t, "cred_hmac", []byte("x"))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	target := credentials.Target{
		Kind: "reload_endpoint", URL: srv.URL, SecretID: "cred_hmac", KeyName: "K",
		GraceSeconds: 1,
	}
	res := Deliver(context.Background(), target, "o", "n", v)
	if res.Success {
		t.Fatal("expected failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call (open only), got %d", calls.Load())
	}
}

func TestDeliverNon2xxFails(t *testing.T) {
	t.Parallel()
	v := newVaultWithSecret(t, "cred_hmac", []byte("x"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	target := credentials.Target{Kind: "webhook", URL: srv.URL, SecretID: "cred_hmac", KeyName: "K"}
	res := Deliver(context.Background(), target, "", "v", v)
	if res.Success {
		t.Fatal("5xx should fail")
	}
	if res.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.Status)
	}
}
