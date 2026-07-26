package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/reactor/internal/registry"
)

// stubValidator satisfies CodeValidator with a programmable result.
type stubValidator struct {
	calls int
	fail  error
}

func (s *stubValidator) Validate(_ context.Context, _, _, _, _ string) error {
	s.calls++
	return s.fail
}

// stubRegistrar satisfies WorkflowRegistrar and records rebuilds. Validation
// compiles in a throwaway temp dir, so a save that does not ALSO rebuild leaves
// the old binary serving every trigger; these counters are what pin that.
type stubRegistrar struct {
	calls    int
	slug     string
	dir      string
	tenantID string
	fail     error
}

func (s *stubRegistrar) RegisterFromDir(_ context.Context, slug, dir, tenantID string) (string, error) {
	s.calls++
	s.slug, s.dir, s.tenantID = slug, dir, tenantID
	if s.fail != nil {
		return "", s.fail
	}
	return "wf_stub", nil
}

type stubCommitter struct{ calls int }

func (s *stubCommitter) Commit(_ context.Context, _, _, _ string) error {
	s.calls++
	return nil
}

func TestSaveCodeRejectsWhenValidatorFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "demo"), 0o700); err != nil {
		t.Fatal(err)
	}
	val := &stubValidator{fail: errors.New("go vet: undefined: foo")}
	srv := &Server{
		WorkflowsRoot: root,
		CodeValidator: val,
	}
	r := chi.NewRouter()
	r.Post("/workflows/{slug}/code", srv.workflowSaveCode)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.PostForm(ts.URL+"/workflows/demo/code", url.Values{"body": {"package main\n"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if val.calls != 1 {
		t.Fatalf("validator called %d times, want 1", val.calls)
	}
	// File must NOT have landed.
	if _, err := os.Stat(filepath.Join(root, "demo", "main.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("main.go should not exist after a failed validate")
	}
}

func TestSaveCodeWritesAndCommitsOnSuccess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	val := &stubValidator{}
	com := &stubCommitter{}
	reg := &stubRegistrar{}
	srv := &Server{
		WorkflowsRoot:    root,
		CodeValidator:    val,
		CodeCommitter:    com,
		WorkflowRegister: reg,
	}
	r := chi.NewRouter()
	r.Post("/workflows/{slug}/code", srv.workflowSaveCode)
	ts := httptest.NewServer(r)
	defer ts.Close()

	body := "package main\n\nfunc main() {}\n"
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.PostForm(ts.URL+"/workflows/demo/code", url.Values{"body": {body}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(root, "demo", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body mismatch:\n--got--\n%s\n--want--\n%s", got, body)
	}
	if com.calls != 1 {
		t.Fatalf("committer called %d times, want 1", com.calls)
	}
	if reg.calls != 1 {
		t.Fatalf("registrar called %d times, want 1: a save that does not rebuild leaves the old binary running", reg.calls)
	}
	if reg.slug != "demo" || reg.dir != filepath.Join(root, "demo") {
		t.Fatalf("rebuild targeted slug=%q dir=%q, want demo / %s", reg.slug, reg.dir, filepath.Join(root, "demo"))
	}
}

// TestSaveCodeRefusesWhenItCannotRebuild pins the trust property: a save that
// cannot recompile must not report success, because the drawer button says
// "Apply + rebuild" and the runtime execs the already-built binary.
func TestSaveCodeRefusesWhenItCannotRebuild(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	srv := &Server{
		WorkflowsRoot: root,
		CodeValidator: &stubValidator{},
		CodeCommitter: &stubCommitter{},
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		// WorkflowRegister deliberately nil.
	}
	r := chi.NewRouter()
	r.Post("/workflows/{slug}/code", srv.workflowSaveCode)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/workflows/demo/code", url.Values{"body": {"package main\nfunc main(){}\n"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusOK {
		t.Fatalf("status = %d: a save that cannot rebuild must not report success", resp.StatusCode)
	}
}

// TestSaveCodeRestoresSourceWhenRebuildFails keeps the on-disk source in step
// with the binary that is actually running.
func TestSaveCodeRestoresSourceWhenRebuildFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := "package main\n\n// ORIGINAL\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		WorkflowsRoot:    root,
		CodeValidator:    &stubValidator{},
		CodeCommitter:    &stubCommitter{},
		WorkflowRegister: &stubRegistrar{fail: errors.New("go build: boom")},
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r := chi.NewRouter()
	r.Post("/workflows/{slug}/code", srv.workflowSaveCode)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/workflows/demo/code", url.Values{"body": {"package main\n\n// REPLACEMENT\nfunc main() {}\n"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("status = %d, want a failure when the rebuild fails", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("source should be rolled back to the version that matches the running binary, got:\n%s", got)
	}
	// The staging file must not be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "stage") || strings.HasSuffix(e.Name(), ".new") {
			t.Fatalf("staging file %q left behind", e.Name())
		}
	}
}

func TestSaveDAGRejectsBadSchema(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	srv := &Server{WorkflowsRoot: root}
	r := chi.NewRouter()
	r.Post("/workflows/{slug}/dag", srv.workflowSaveDAG)
	ts := httptest.NewServer(r)
	defer ts.Close()

	bad := `{"slug":"../escape","version":"0.1.0","steps":[]}`
	resp, err := http.Post(ts.URL+"/workflows/escape/dag", "application/json", strings.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestSaveDAGAcceptsCanonicalShape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	srv := &Server{WorkflowsRoot: root}
	r := chi.NewRouter()
	r.Post("/workflows/{slug}/dag", srv.workflowSaveDAG)
	ts := httptest.NewServer(r)
	defer ts.Close()

	good := `{"slug":"demo","version":"0.1.0","steps":[{"name":"a","kind":"step"}]}`
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Post(ts.URL+"/workflows/demo/dag", "application/json", strings.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(root, "demo", "dag.json"))
	if err != nil {
		t.Fatalf("read dag.json: %v", err)
	}
	if string(got) != good {
		t.Fatalf("body mismatch")
	}
}

func TestSaveCodeOversizeReturns413(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	srv := &Server{WorkflowsRoot: root, CodeValidator: &stubValidator{}}
	r := chi.NewRouter()
	r.Post("/workflows/{slug}/code", srv.workflowSaveCode)
	ts := httptest.NewServer(r)
	defer ts.Close()

	big := strings.Repeat("x", 2<<20) // 2 MiB > 1 MiB cap
	resp, err := http.Post(ts.URL+"/workflows/demo/code", "application/octet-stream", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// _ keeps registry imported in case the test file later uses it directly.
var _ = registry.ValidateDAG
