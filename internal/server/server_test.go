package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	tlsPkg "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/brightinteraction/reactor/internal/credentials"
	"github.com/brightinteraction/reactor/internal/migrate"
	"github.com/brightinteraction/reactor/internal/registry"
	"github.com/brightinteraction/reactor/internal/runtime/journal"
	_ "modernc.org/sqlite"
)

// getBody is a tiny GET helper that handles the err check + defer
// Body.Close in one place. Without it, every test does the
// `resp, _ := http.Get; defer resp.Body.Close()` dance which `go vet`
// flags as "using resp before checking for errors".
func getBody(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return body
}

// getStatus returns just the status code; used by the 404 paths that
// don't care about the body.
func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// getStatusBody returns both for the timeline test that asserts
// status == 200 AND a substring on the body.
func getStatusBody(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, body
}

// genSelfSignedCert writes a fresh ECDSA self-signed cert + key to a
// temp dir and returns the file paths. Used only by the TLS test;
// avoids a fixture cert that would expire over time.
func genSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "reactor-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	defer certPEM.Close()
	if err := pem.Encode(certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer keyPEM.Close()
	if err := pem.Encode(keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// TestRunWithTLSServesHTTPS boots the server with a self-signed cert,
// hits /healthz over HTTPS via a client that trusts the cert, asserts
// 200 + JSON shape + HSTS header (which only fires on real TLS).
func TestRunWithTLSServesHTTPS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tls.db")
	url := "sqlite://" + dbPath
	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := migrate.Up(context.Background(), silent, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	j := journal.New(db, journal.EngineSQLite)
	cred := credentials.New(db, credentials.EngineSQLite)
	reg := registry.New(filepath.Join(dir, "workflows"))

	srv := &Server{
		Journal: j, Credentials: cred, Registry: reg, Log: silent, Version: "tls-test",
		BasicAuth: BasicAuthConfig{AllowNoAuth: true},
	}

	// Bind to :0 so the OS picks a free port; we recover it after.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	certPath, keyPath := genSelfSignedCert(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- srv.RunWithTLS(ctx, addr, TLSConfig{CertFile: certPath, KeyFile: keyPath})
	}()

	// Wait for listener.
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tlsPkg.Config{InsecureSkipVerify: true}},
		Timeout:   500 * time.Millisecond,
	}
	for time.Now().Before(deadline) {
		var err error
		resp, err = client.Get("https://" + addr + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("server never accepted a TLS connection")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if v := resp.Header.Get("Strict-Transport-Security"); v == "" {
		t.Fatal("HSTS header missing on TLS response")
	}
	cancel()
	<-done
}

// newTestServer wires the same components the daemon uses so the
// integration test exercises the production assembly. Webhook receiver
// is omitted because that path needs a vault.Store + Dispatcher; the
// pure status-page tests don't care.
func newTestServer(t *testing.T) (*httptest.Server, *journal.Journal, *credentials.Repo) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "s.db")
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
	cred := credentials.New(db, credentials.EngineSQLite)
	reg := registry.New(filepath.Join(dir, "workflows"))

	s := &Server{
		Journal:     j,
		Credentials: cred,
		Registry:    reg,
		Log:         silent,
		Version:     "test",
		BasicAuth:   BasicAuthConfig{AllowNoAuth: true},
	}
	r := chi.NewRouter()
	s.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, j, cred
}

func TestHealthzReturnsOK(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["version"] != "test" {
		t.Fatalf("unexpected body: %v", body)
	}
}

func TestHomePageRedirectsToOnboardingOnFirstBoot(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	// Don't follow redirects; the assertion is that /home itself
	// returns 303 to /onboarding when no workflows exist.
	cl := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := cl.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/onboarding" {
		t.Fatalf("location = %q, want /onboarding", loc)
	}
}

func TestOnboardingPageRendersFiveSteps(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	body := getBody(t, srv.URL+"/onboarding")
	for _, want := range []string{"Step 1:", "Step 2:", "Step 3:", "Step 4:", "Step 5:"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q in onboarding body", want)
		}
	}
}

func TestHomePageRendersWorkflowAndRun(t *testing.T) {
	t.Parallel()
	srv, j, _ := newTestServer(t)
	ctx := context.Background()

	if err := j.CreateWorkflow(ctx, "wf_demo", "demo", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateRun(ctx, "run_demo", "wf_demo", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkRunFinished(ctx, "run_demo", "succeeded"); err != nil {
		t.Fatal(err)
	}

	body := getBody(t, srv.URL+"/")
	if !strings.Contains(string(body), `<code>demo</code>`) {
		t.Fatal("expected workflow slug 'demo' in home page")
	}
	if !strings.Contains(string(body), "tag-succeeded") {
		t.Fatal("expected succeeded tag in recent runs")
	}
}

func TestRunDetailRendersTimeline(t *testing.T) {
	t.Parallel()
	srv, j, _ := newTestServer(t)
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_demo", "demo", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateRun(ctx, "run_t", "wf_demo", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordStepStart(ctx, "run_t", "fetch", 1, "k", "h"); err != nil {
		t.Fatal(err)
	}
	_ = j.RecordStepEnd(ctx, "run_t", "fetch", 1, json.RawMessage(`"ok"`), "")
	_ = j.MarkRunFinished(ctx, "run_t", "succeeded")

	status, body := getStatusBody(t, srv.URL+"/runs/run_t")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(string(body), `<code>fetch</code>`) {
		t.Fatal("expected step name 'fetch' in run detail")
	}
}

func TestRunsListPagination(t *testing.T) {
	t.Parallel()
	srv, j, _ := newTestServer(t)
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_a", "a", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateWorkflow(ctx, "wf_b", "b", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	for i, runID := range []string{"run_a1", "run_a2", "run_a3"} {
		if err := j.CreateRun(ctx, runID, "wf_a", "manual", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	if err := j.CreateRun(ctx, "run_b1", "wf_b", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// Limit=2 + workflow filter shows only matching runs + a Next link.
	body := string(getBody(t, srv.URL+"/runs?workflow_id=wf_a&limit=2"))
	if !strings.Contains(body, "run_a") {
		t.Fatal("expected wf_a runs in body")
	}
	if strings.Contains(body, "run_b1") {
		t.Fatal("wf_b run leaked through filter")
	}
	if !strings.Contains(body, "Next") {
		t.Fatal("expected Next pager link")
	}

	// Page 2 = remainder + Prev link visible.
	body2 := string(getBody(t, srv.URL+"/runs?workflow_id=wf_a&limit=2&offset=2"))
	if !strings.Contains(body2, "Prev") {
		t.Fatal("expected Prev link on page 2")
	}
}

func TestRunDetailMissingReturns404(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	if status := getStatus(t, srv.URL+"/runs/run_does_not_exist"); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestCredentialsPage(t *testing.T) {
	t.Parallel()
	srv, _, cred := newTestServer(t)
	if err := cred.Create(context.Background(), credentials.CreateParams{
		ID: "cred_demo", Name: "demo-cred", Service: "x", Provider: "shared-secret",
		AutoRotate: true, RotationIntervalDays: 30,
	}); err != nil {
		t.Fatal(err)
	}
	body := getBody(t, srv.URL+"/credentials")
	if !strings.Contains(string(body), "demo-cred") || !strings.Contains(string(body), "tag-on") {
		t.Fatalf("expected demo-cred + auto-on tag in credentials page; got %d bytes", len(body))
	}
}

func TestCredentialDetailPage(t *testing.T) {
	t.Parallel()
	srv, _, cred := newTestServer(t)
	ctx := context.Background()
	if err := cred.Create(ctx, credentials.CreateParams{
		ID: "cred_demo", Name: "demo-cred", Service: "x", Provider: "shared-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cred.AppendAudit(ctx, credentials.AuditEntry{
		CredentialID: "cred_demo",
		Action:       "rotate.success",
		ActorKind:    "scheduler",
		Detail:       json.RawMessage(`{"k":"v"}`),
	}); err != nil {
		t.Fatal(err)
	}
	body := getBody(t, srv.URL+"/credentials/cred_demo")
	if !strings.Contains(string(body), "rotate.success") {
		t.Fatal("expected rotate.success in audit table")
	}
}

func TestCredentialDetailMissingReturns404(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	if status := getStatus(t, srv.URL+"/credentials/cred_missing"); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}
