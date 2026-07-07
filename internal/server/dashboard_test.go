package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/brightinteraction/reactor/internal/registry"
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
	srv := &Server{
		WorkflowsRoot: root,
		CodeValidator: val,
		CodeCommitter: com,
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
