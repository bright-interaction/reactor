package rotators

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/nacl/box"

	"github.com/brightinteraction/reactor/internal/credentials"
)

func TestDeliverFileWriteAtomicallyReplaces(t *testing.T) {
	// Not parallel: file_write containment reads REACTOR_FILE_WRITE_ROOT from
	// the process env, and t.Setenv is incompatible with t.Parallel.
	dir := t.TempDir()
	t.Setenv("REACTOR_FILE_WRITE_ROOT", dir)
	path := filepath.Join(dir, "secret.env")
	if err := os.WriteFile(path, []byte("OLD"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	target := credentials.Target{Kind: "file_write", URL: path}
	res := Deliver(context.Background(), target, "OLD", "NEW-VALUE", nil)
	if !res.Success {
		t.Fatalf("delivery failed: %s", res.Error)
	}

	// Containment regression: a traversal target outside the root is refused.
	escape := credentials.Target{Kind: "file_write", URL: filepath.Join(dir, "..", "escape.env")}
	if r := Deliver(context.Background(), escape, "OLD", "X", nil); r.Success {
		t.Fatalf("file_write should refuse a target outside REACTOR_FILE_WRITE_ROOT")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "NEW-VALUE" {
		t.Fatalf("file contents = %q, want NEW-VALUE", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", info.Mode().Perm())
	}

	matches, _ := filepath.Glob(filepath.Join(dir, ".reactor-rotate-*"))
	if len(matches) != 0 {
		t.Fatalf("temp file leak: %v", matches)
	}
}

func TestDeliverFileWriteMissingPath(t *testing.T) {
	t.Parallel()

	target := credentials.Target{Kind: "file_write"}
	res := Deliver(context.Background(), target, "", "v", nil)
	if res.Success {
		t.Fatalf("want failure on missing URL, got success")
	}
	if !strings.Contains(res.Error, "missing url") {
		t.Fatalf("error = %q, want 'missing url'", res.Error)
	}
}

func TestDeliverForgejoSecretHappyPath(t *testing.T) {
	t.Parallel()

	const apiToken = "forgejo-token-xyz"
	v := newVaultWithSecret(t, "cred_forgejo_token", []byte(apiToken))

	var hits atomic.Int32
	var seenPath, seenAuth, seenBody string
	var seenMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	target := credentials.Target{
		Kind:     "forgejo_secret",
		URL:      srv.URL + "/api/v1/repos/owner/repo",
		KeyName:  "DEPLOY_KEY",
		SecretID: "cred_forgejo_token",
	}
	res := Deliver(context.Background(), target, "old", "new-secret-value", v)
	if !res.Success {
		t.Fatalf("delivery failed: status=%d err=%s", res.Status, res.Error)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	if seenMethod != http.MethodPut {
		t.Fatalf("method = %q, want PUT", seenMethod)
	}
	if want := "/api/v1/repos/owner/repo/actions/secrets/DEPLOY_KEY"; seenPath != want {
		t.Fatalf("path = %q, want %q", seenPath, want)
	}
	if seenAuth != "token "+apiToken {
		t.Fatalf("auth = %q, want token "+apiToken, seenAuth)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(seenBody), &body); err != nil {
		t.Fatalf("body parse: %v (raw %q)", err, seenBody)
	}
	if body["data"] != "new-secret-value" {
		t.Fatalf("body.data = %q, want new-secret-value", body["data"])
	}
}

func TestDeliverForgejoSecretHTTPErrorFails(t *testing.T) {
	t.Parallel()

	v := newVaultWithSecret(t, "cred_t", []byte("tok"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	target := credentials.Target{
		Kind:     "forgejo_secret",
		URL:      srv.URL,
		KeyName:  "K",
		SecretID: "cred_t",
	}
	res := Deliver(context.Background(), target, "", "new", v)
	if res.Success {
		t.Fatalf("want failure on 401")
	}
	if res.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Status)
	}
}

func TestDeliverForgejoSecretMissingFields(t *testing.T) {
	t.Parallel()

	target := credentials.Target{Kind: "forgejo_secret"}
	res := Deliver(context.Background(), target, "", "v", nil)
	if res.Success || !strings.Contains(res.Error, "missing") {
		t.Fatalf("want missing-field error, got success=%v err=%q", res.Success, res.Error)
	}
}

func TestDeliverGitHubSecretHappyPath(t *testing.T) {
	t.Parallel()

	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	const keyID = "568250167242549743"

	const apiToken = "ghp_REDACTED"
	v := newVaultWithSecret(t, "cred_gh", []byte(apiToken))

	var pubKeyHits, putHits atomic.Int32
	var seenAuth, seenAPIVer string
	var receivedCiphertextB64, receivedKeyID string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/owner/repo/actions/secrets/public-key", func(w http.ResponseWriter, r *http.Request) {
		pubKeyHits.Add(1)
		seenAuth = r.Header.Get("Authorization")
		seenAPIVer = r.Header.Get("X-GitHub-Api-Version")
		fmt.Fprintf(w, `{"key_id":%q,"key":%q}`,
			keyID, base64.StdEncoding.EncodeToString(pub[:]))
	})
	mux.HandleFunc("/api/v3/repos/owner/repo/actions/secrets/DEPLOY_KEY", func(w http.ResponseWriter, r *http.Request) {
		putHits.Add(1)
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			EncryptedValue string `json:"encrypted_value"`
			KeyID          string `json:"key_id"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("parse PUT body: %v", err)
		}
		receivedCiphertextB64 = parsed.EncryptedValue
		receivedKeyID = parsed.KeyID
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	target := credentials.Target{
		Kind:     "github_secret",
		URL:      srv.URL + "/api/v3/repos/owner/repo",
		KeyName:  "DEPLOY_KEY",
		SecretID: "cred_gh",
	}
	const plaintext = "rotated-secret-value-xyz"
	res := Deliver(context.Background(), target, "", plaintext, v)
	if !res.Success {
		t.Fatalf("delivery failed: status=%d err=%s", res.Status, res.Error)
	}
	if pubKeyHits.Load() != 1 || putHits.Load() != 1 {
		t.Fatalf("hits: pubKey=%d put=%d, want 1+1", pubKeyHits.Load(), putHits.Load())
	}
	if seenAuth != "Bearer "+apiToken {
		t.Fatalf("auth = %q", seenAuth)
	}
	if seenAPIVer != "2022-11-28" {
		t.Fatalf("api version = %q, want 2022-11-28", seenAPIVer)
	}
	if receivedKeyID != keyID {
		t.Fatalf("key_id = %q, want %q", receivedKeyID, keyID)
	}

	sealed, err := base64.StdEncoding.DecodeString(receivedCiphertextB64)
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	decrypted, ok := box.OpenAnonymous(nil, sealed, pub, priv)
	if !ok {
		t.Fatalf("OpenAnonymous failed")
	}
	if string(decrypted) != plaintext {
		t.Fatalf("decrypted = %q, want %q", decrypted, plaintext)
	}
}

func TestDeliverGitHubSecretPublicKeyHTTPError(t *testing.T) {
	t.Parallel()

	v := newVaultWithSecret(t, "cred_gh", []byte("tok"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	target := credentials.Target{
		Kind: "github_secret", URL: srv.URL,
		KeyName: "K", SecretID: "cred_gh",
	}
	res := Deliver(context.Background(), target, "", "v", v)
	if res.Success || !strings.Contains(res.Error, "401") {
		t.Fatalf("want 401 error, got success=%v err=%q", res.Success, res.Error)
	}
}

func TestDeliverGitHubSecretMissingFields(t *testing.T) {
	t.Parallel()

	target := credentials.Target{Kind: "github_secret"}
	res := Deliver(context.Background(), target, "", "v", nil)
	if res.Success || !strings.Contains(res.Error, "missing") {
		t.Fatalf("want missing-field error, got success=%v err=%q", res.Success, res.Error)
	}
}

func TestDeliverDockyardVaultHappyPath(t *testing.T) {
	t.Parallel()

	const apiToken = "dy_token_xxxxxxxxx"
	v := newVaultWithSecret(t, "cred_dy", []byte(apiToken))

	var hits atomic.Int32
	var seenMethod, seenPath, seenAuth, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := credentials.Target{
		Kind:     "dockyard_vault",
		URL:      srv.URL + "/api/vault/vault_abc123",
		SecretID: "cred_dy",
	}
	res := Deliver(context.Background(), target, "old", "new-rotated-value", v)
	if !res.Success {
		t.Fatalf("delivery failed: status=%d err=%s", res.Status, res.Error)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	if seenMethod != http.MethodPut {
		t.Fatalf("method = %q, want PUT", seenMethod)
	}
	if seenPath != "/api/vault/vault_abc123" {
		t.Fatalf("path = %q", seenPath)
	}
	if seenAuth != "Bearer "+apiToken {
		t.Fatalf("auth = %q", seenAuth)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(seenBody), &parsed); err != nil {
		t.Fatalf("body parse: %v (raw %q)", err, seenBody)
	}
	if parsed["value"] != "new-rotated-value" {
		t.Fatalf("body.value = %q", parsed["value"])
	}
}

func TestDeliverDockyardVaultHTTPError(t *testing.T) {
	t.Parallel()

	v := newVaultWithSecret(t, "cred_dy", []byte("tok"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	target := credentials.Target{
		Kind: "dockyard_vault", URL: srv.URL + "/api/vault/v1",
		SecretID: "cred_dy",
	}
	res := Deliver(context.Background(), target, "", "v", v)
	if res.Success || res.Status != http.StatusForbidden {
		t.Fatalf("want 403, got success=%v status=%d", res.Success, res.Status)
	}
}

func TestDeliverDockyardVaultMissingFields(t *testing.T) {
	t.Parallel()

	target := credentials.Target{Kind: "dockyard_vault"}
	res := Deliver(context.Background(), target, "", "v", nil)
	if res.Success || !strings.Contains(res.Error, "missing") {
		t.Fatalf("want missing-field error, got success=%v err=%q", res.Success, res.Error)
	}
}
