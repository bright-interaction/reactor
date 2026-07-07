package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/brightinteraction/reactor/internal/migrate"
	"github.com/brightinteraction/reactor/internal/runtime/journal"
	"github.com/brightinteraction/reactor/internal/vault"
	_ "modernc.org/sqlite"
)

// captureDispatcher records every Dispatch call so tests can assert on
// payload + trigger identity.
type captureDispatcher struct {
	calls atomic.Int32
	last  []byte
	trig  journal.Trigger
}

func (c *captureDispatcher) Dispatch(_ context.Context, t journal.Trigger, payload []byte) error {
	c.calls.Add(1)
	c.last = append([]byte(nil), payload...)
	c.trig = t
	return nil
}

func (c *captureDispatcher) DispatchSync(_ context.Context, t journal.Trigger, payload []byte, _ time.Duration) (string, string, json.RawMessage, error) {
	c.calls.Add(1)
	c.last = append([]byte(nil), payload...)
	c.trig = t
	return "run_sync", "succeeded", json.RawMessage(`{"ok":true}`), nil
}

func newTestReceiver(t *testing.T, secretValue string) (*Receiver, *captureDispatcher, *journal.Journal, string) {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wh.db")
	url := "sqlite://" + dbPath

	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := migrate.Up(context.Background(), silent, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	j := journal.New(db, journal.EngineSQLite)

	if err := j.CreateWorkflow(context.Background(), "wf_1", "demo", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("workflow: %v", err)
	}

	v, err := vault.NewStore(vault.NewMemoryBackend(), bytesN(32, 0xAB))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	if err := v.Put(context.Background(), "cred_hmac", []byte(secretValue)); err != nil {
		t.Fatalf("vault put: %v", err)
	}

	tokenID, _ := journal.NewTokenID()
	if _, err := j.CreateWebhookTrigger(context.Background(), "wf_1", tokenID, "cred_hmac", "generic", []byte(`{}`)); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	disp := &captureDispatcher{}
	r := &Receiver{
		Journal: j,
		Vault:   v,
		Disp:    disp,
		Log:     silent,
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	return r, disp, j, tokenID
}

func bytesN(n int, b byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookGenericHappyPath(t *testing.T) {
	t.Parallel()
	r, disp, _, tok := newTestReceiver(t, "shhh")

	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := []byte(`{"event":"order.created","id":"ord_1"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+tok, bytes.NewReader(body))
	req.Header.Set("X-Webhook-Signature", "sha256="+sign([]byte("shhh"), body))
	req.Header.Set("X-Webhook-Delivery", "evt_42")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d %s", resp.StatusCode, buf)
	}
	if disp.calls.Load() != 1 {
		t.Fatalf("dispatch count = %d, want 1", disp.calls.Load())
	}
	if string(disp.last) != string(body) {
		t.Fatalf("dispatched body mismatch")
	}
}

func TestWebhookSyncReturnsOutput(t *testing.T) {
	t.Parallel()
	r, _, j, _ := newTestReceiver(t, "shhh")
	// Add a SYNCHRONOUS webhook trigger.
	syncTok, _ := journal.NewTokenID()
	if _, err := j.CreateWebhookTrigger(context.Background(), "wf_1", syncTok, "cred_hmac", "generic", []byte(`{"sync":true,"timeout_seconds":5}`)); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := []byte(`{"q":"hello"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+syncTok, bytes.NewReader(body))
	req.Header.Set("X-Webhook-Signature", "sha256="+sign([]byte("shhh"), body))
	req.Header.Set("X-Webhook-Delivery", "sync_1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The fake DispatchSync returns succeeded + {"ok":true}, so the caller gets
	// 200 with the run result inline.
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("sync webhook got %d %s, want 200", resp.StatusCode, buf)
	}
	var out struct {
		RunID  string          `json:"run_id"`
		Status string          `json:"status"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.RunID != "run_sync" || out.Status != "succeeded" || string(out.Output) != `{"ok":true}` {
		t.Fatalf("sync response = %+v", out)
	}
}

func TestParseSyncConfig(t *testing.T) {
	t.Parallel()
	if sc := parseSyncConfig([]byte(`{}`)); sc.Sync {
		t.Fatal("empty config should not be sync")
	}
	if sc := parseSyncConfig([]byte(`{"sync":true}`)); !sc.Sync || sc.TimeoutSeconds != 30 {
		t.Fatalf("default timeout wrong: %+v", sc)
	}
	if sc := parseSyncConfig([]byte(`{"sync":true,"timeout_seconds":999}`)); sc.TimeoutSeconds != 120 {
		t.Fatalf("timeout should clamp to 120, got %d", sc.TimeoutSeconds)
	}
}

func TestWebhookBadSignature(t *testing.T) {
	t.Parallel()
	r, disp, _, tok := newTestReceiver(t, "shhh")
	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := []byte(`{}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+tok, bytes.NewReader(body))
	req.Header.Set("X-Webhook-Signature", "sha256=00000000")
	req.Header.Set("X-Webhook-Delivery", "evt_bad")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
	if disp.calls.Load() != 0 {
		t.Fatal("dispatch fired on bad signature")
	}
}

func TestWebhookDedupReplay(t *testing.T) {
	t.Parallel()
	r, disp, _, tok := newTestReceiver(t, "shhh")
	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := []byte(`{"id":"evt_dup"}`)
	sig := "sha256=" + sign([]byte("shhh"), body)

	post := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+tok, bytes.NewReader(body))
		req.Header.Set("X-Webhook-Signature", sig)
		req.Header.Set("X-Webhook-Delivery", "evt_dup")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if post() != http.StatusAccepted {
		t.Fatal("first delivery should be accepted")
	}
	if post() != http.StatusOK {
		t.Fatal("replay should be 200 (dedup)")
	}
	if disp.calls.Load() != 1 {
		t.Fatalf("dispatch fired %d times, want 1", disp.calls.Load())
	}
}

func TestWebhookUnknownToken(t *testing.T) {
	t.Parallel()
	r, _, _, _ := newTestReceiver(t, "x")
	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/whk_doesnotexist", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
}

func TestWebhookGitHubVerify(t *testing.T) {
	t.Parallel()
	body := []byte(`{"action":"opened"}`)
	key := []byte("secret123")
	good := "sha256=" + sign(key, body)
	if err := verifyGitHub(good, body, key); err != nil {
		t.Fatalf("good sig rejected: %v", err)
	}
	if err := verifyGitHub("sha256=00", body, key); err == nil {
		t.Fatal("bad sig accepted")
	}
	if err := verifyGitHub("md5=abc", body, key); err == nil {
		t.Fatal("non-sha256 prefix accepted")
	}
}

func TestWebhookStripeVerify(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"evt_test"}`)
	key := []byte("whsec_xyz")
	now := time.Unix(1_700_000_000, 0)

	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%d.", now.Unix())
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%d,v1=%s", now.Unix(), sig)

	if err := verifyStripe(header, body, key, now); err != nil {
		t.Fatalf("good sig rejected: %v", err)
	}
	// Stale timestamp.
	if err := verifyStripe(header, body, key, now.Add(10*time.Minute)); err == nil {
		t.Fatal("stale timestamp accepted")
	}
	// Bad signature.
	bad := fmt.Sprintf("t=%d,v1=00000000", now.Unix())
	if err := verifyStripe(bad, body, key, now); err == nil {
		t.Fatal("bad sig accepted")
	}
}

func TestWebhookOversizeBodyRejected(t *testing.T) {
	t.Parallel()
	r, disp, _, tok := newTestReceiver(t, "shhh")
	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	huge := bytes.Repeat([]byte("a"), MaxBodyBytes+10)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+tok, bytes.NewReader(huge))
	req.Header.Set("X-Webhook-Signature", "sha256="+sign([]byte("shhh"), huge))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body: status = %d, want 413", resp.StatusCode)
	}
	if disp.calls.Load() != 0 {
		t.Fatal("dispatch fired on oversize body")
	}
}
