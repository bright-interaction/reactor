package codegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAnthropic returns a httptest.Server that responds with a single
// tool_use block containing the given EmitInput. Successive responses
// are pulled from the responses queue; once exhausted, returns an error.
func fakeAnthropic(t *testing.T, responses []EmitInput) (*AnthropicClient, *httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	idx := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("x-api-key") == "" {
			http.Error(w, `{"error":{"type":"authentication_error","message":"missing api key"}}`, http.StatusUnauthorized)
			return
		}
		if idx >= len(responses) {
			http.Error(w, `{"error":{"type":"server_error","message":"no more responses"}}`, 500)
			return
		}
		body, err := json.Marshal(responses[idx])
		if err != nil {
			http.Error(w, "marshal", 500)
			return
		}
		idx++
		resp := MessagesResponse{
			ID:         "msg_test",
			Model:      DefaultModel,
			Role:       "assistant",
			StopReason: "tool_use",
			Content: []ContentBlock{{
				Type:  "tool_use",
				ID:    "toolu_test",
				Name:  EmitToolName,
				Input: body,
			}},
			Usage: Usage{InputTokens: 100, OutputTokens: 200},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	return &AnthropicClient{
		HTTPClient: srv.Client(),
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Version:    DefaultAPIVersion,
	}, srv, calls
}

// fakeValidator + fakeCommitter let tests bypass the real go toolchain.
type fakeValidator struct {
	failTimes int
	calls     atomic.Int32
}

func (f *fakeValidator) Validate(_ context.Context, _ string, _ EmitInput) error {
	n := f.calls.Add(1)
	if int(n) <= f.failTimes {
		return errors.New("synthetic validation failure")
	}
	return nil
}

type fakeCommitter struct{ calls atomic.Int32 }

func (f *fakeCommitter) Commit(_ context.Context, _ string, _ string, _ string) error {
	f.calls.Add(1)
	return nil
}

func TestGenerateHappyPath(t *testing.T) {
	t.Parallel()

	emit := EmitInput{
		Slug:         "welcome-customer",
		Version:      "0.1.0",
		WorkflowGo:   "package main\n",
		DAGJson:      `{"nodes":[],"edges":[],"triggers":[]}`,
		WorkflowTest: "package main\n",
	}
	client, srv, _ := fakeAnthropic(t, []EmitInput{emit})
	defer srv.Close()

	dir := t.TempDir()
	val := &fakeValidator{}
	com := &fakeCommitter{}
	g := &Generator{
		Anthropic:    client,
		WorkflowsDir: dir,
		Validator:    val,
		Committer:    com,
	}
	res, err := g.Generate(context.Background(), GenerateRequest{Brief: "send a welcome email"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "welcome-customer" {
		t.Fatalf("got %q", res.Slug)
	}
	if res.Attempts != 1 {
		t.Fatalf("attempts %d, want 1", res.Attempts)
	}
	if com.calls.Load() != 1 {
		t.Fatalf("committer calls %d, want 1", com.calls.Load())
	}
	// Files materialised.
	for _, fn := range []string{"workflow.go", "dag.json", "workflow_test.go"} {
		if _, err := os.Stat(filepath.Join(dir, "welcome-customer", fn)); err != nil {
			t.Fatalf("expected %s: %v", fn, err)
		}
	}
}

func TestGenerateLensInjectsKnowledgeAndGraph(t *testing.T) {
	t.Parallel()
	emit := EmitInput{
		Slug:         "send-welcome",
		Version:      "0.1.0",
		WorkflowGo:   "package main\n",
		DAGJson:      `{"nodes":[],"edges":[],"triggers":[]}`,
		WorkflowTest: "package main\n",
	}
	client, srv, _ := fakeAnthropic(t, []EmitInput{emit})
	defer srv.Close()

	citationCount := 0
	captured := ""
	lens := &PromptLens{
		KnowledgeLimit: 3,
		Search: func(ctx context.Context, query string, limit int) ([]LensHit, error) {
			return []LensHit{{
				ID:    "h_timeout",
				Topic: "http-clients",
				Title: "Always pass context.Context with a timeout",
				Body:  "Use http.NewRequestWithContext + defer cancel.",
				Score: 5.5,
				Gold:  true,
			}}, nil
		},
		QueryGraph: func(query string) string {
			return "NODE workflow:welcome-customer [status=succeeded]\nNODE credential:resend [provider=shared-secret]\nEDGE workflow:welcome-customer USES credential:resend\n"
		},
		IncrementCitation: func(_ context.Context, id string) error {
			if id == "h_timeout" {
				citationCount++
			}
			return nil
		},
	}

	g := &Generator{
		Anthropic:    client,
		WorkflowsDir: t.TempDir(),
		Validator:    &fakeValidator{},
		Committer:    &fakeCommitter{},
		Lens:         lens,
		EchoPrompt:   func(p string) { captured = p },
	}
	if _, err := g.Generate(context.Background(), GenerateRequest{Brief: "send a welcome email when a user signs up"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(captured, "Always pass context.Context with a timeout") {
		t.Errorf("knowledge title missing from prompt; got:\n%s", captured)
	}
	if !strings.Contains(captured, "GOLD") {
		t.Errorf("gold tag missing from prompt; got:\n%s", captured)
	}
	if !strings.Contains(captured, "NODE workflow:welcome-customer") {
		t.Errorf("graph slice missing from prompt; got:\n%s", captured)
	}
	if !strings.Contains(captured, "EDGE workflow:welcome-customer USES credential:resend") {
		t.Errorf("graph edge missing from prompt; got:\n%s", captured)
	}
	if citationCount != 1 {
		t.Errorf("citation bump count = %d, want 1", citationCount)
	}
}

func TestGenerateLensIsOptional(t *testing.T) {
	t.Parallel()
	emit := EmitInput{
		Slug:         "no-lens",
		Version:      "0.1.0",
		WorkflowGo:   "package main\n",
		DAGJson:      `{"nodes":[],"edges":[],"triggers":[]}`,
		WorkflowTest: "package main\n",
	}
	client, srv, _ := fakeAnthropic(t, []EmitInput{emit})
	defer srv.Close()

	captured := ""
	g := &Generator{
		Anthropic:    client,
		WorkflowsDir: t.TempDir(),
		Validator:    &fakeValidator{},
		Committer:    &fakeCommitter{},
		EchoPrompt:   func(p string) { captured = p },
		// Lens nil -> brief-only prompt
	}
	if _, err := g.Generate(context.Background(), GenerateRequest{Brief: "trivial brief"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(captured, "Runtime graph slice") || strings.Contains(captured, "Relevant knowledge") {
		t.Errorf("lens-disabled prompt should not contain lens sections; got:\n%s", captured)
	}
}

func TestGenerateRetryOnValidationFail(t *testing.T) {
	t.Parallel()

	emit := EmitInput{
		Slug:         "demo",
		Version:      "0.1.0",
		WorkflowGo:   "package main\n",
		DAGJson:      `{}`,
		WorkflowTest: "package main\n",
	}
	// Both calls return the same content; validator fails first time.
	client, srv, calls := fakeAnthropic(t, []EmitInput{emit, emit})
	defer srv.Close()

	dir := t.TempDir()
	val := &fakeValidator{failTimes: 1}
	g := &Generator{
		Anthropic:    client,
		WorkflowsDir: dir,
		Validator:    val,
		Committer:    &fakeCommitter{},
	}
	res, err := g.Generate(context.Background(), GenerateRequest{Brief: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", res.Attempts)
	}
	if calls.Load() != 2 {
		t.Fatalf("API calls = %d, want 2", calls.Load())
	}
}

func TestGenerateGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()
	emit := EmitInput{
		Slug:         "demo",
		Version:      "0.1.0",
		WorkflowGo:   "package main\n",
		DAGJson:      `{}`,
		WorkflowTest: "package main\n",
	}
	client, srv, _ := fakeAnthropic(t, []EmitInput{emit, emit, emit})
	defer srv.Close()

	g := &Generator{
		Anthropic:    client,
		WorkflowsDir: t.TempDir(),
		Validator:    &fakeValidator{failTimes: 99},
		Committer:    &fakeCommitter{},
		MaxRetries:   3,
	}
	if _, err := g.Generate(context.Background(), GenerateRequest{Brief: "demo"}); err == nil {
		t.Fatal("expected error after max retries")
	}
}

func TestAnthropicAPIErrorTyped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()

	c := &AnthropicClient{
		HTTPClient: srv.Client(),
		APIKey:     "k",
		BaseURL:    srv.URL,
		Version:    DefaultAPIVersion,
	}
	_, err := c.SendMessages(context.Background(), MessagesRequest{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	apiErr := &APIError{}
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want APIError", err)
	}
	if !apiErr.IsRateLimited() {
		t.Fatalf("not flagged as rate limited: %v", apiErr)
	}
}

func TestLintForbidsTimeSleep(t *testing.T) {
	t.Parallel()
	src := `package main

import "time"

func main() {
	time.Sleep(time.Second)
}
`
	if err := lintWorkflow(src); err == nil || !strings.Contains(err.Error(), "time.Sleep") {
		t.Fatalf("got %v, want time.Sleep error", err)
	}
}

func TestLintForbidsTimeNow(t *testing.T) {
	t.Parallel()
	src := `package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Println(time.Now())
}
`
	if err := lintWorkflow(src); err == nil || !strings.Contains(err.Error(), "time.Now") {
		t.Fatalf("got %v, want time.Now error", err)
	}
}

func TestExtractEmitRejectsTraversalSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		slug string
	}{
		{"parent ref", "../escape"},
		{"absolute path", "/etc/passwd"},
		{"backslash", "evil\\thing"},
		{"empty", ""},
		{"capital letters", "Evil"},
		{"starts with digit", "1bad"},
		{"underscore", "bad_one"},
		{"dot", "bad.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(EmitInput{Slug: tc.slug, Version: "0.1.0"})
			resp := &MessagesResponse{
				Content: []ContentBlock{{
					Type:  "tool_use",
					ID:    "toolu_x",
					Name:  EmitToolName,
					Input: body,
				}},
			}
			if _, err := extractEmit(resp); err == nil {
				t.Fatalf("slug %q should be rejected by extractEmit (filepath.Join would walk outside WorkflowsDir)", tc.slug)
			}
		})
	}
}

func TestExtractEmitAcceptsValidSlug(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(EmitInput{Slug: "send-welcome-email", Version: "0.1.0"})
	resp := &MessagesResponse{
		Content: []ContentBlock{{
			Type:  "tool_use",
			ID:    "toolu_x",
			Name:  EmitToolName,
			Input: body,
		}},
	}
	if _, err := extractEmit(resp); err != nil {
		t.Fatalf("valid slug rejected: %v", err)
	}
}

func TestLintAllowsTimeNowInsideClosure(t *testing.T) {
	t.Parallel()
	src := `package main

import (
	"context"
	"time"
)

func Run(ctx context.Context) error {
	_ = func(_ context.Context) (int64, error) {
		return time.Now().Unix(), nil
	}
	return nil
}
`
	if err := lintWorkflow(src); err != nil {
		t.Fatalf("time.Now inside closure must be allowed (Step caches output for replay), got %v", err)
	}
}

func TestLintStillForbidsTimeSleepInsideClosure(t *testing.T) {
	t.Parallel()
	src := `package main

import (
	"context"
	"time"
)

func Run(ctx context.Context) error {
	_ = func(_ context.Context) error {
		time.Sleep(1 * time.Second)
		return nil
	}
	return nil
}
`
	if err := lintWorkflow(src); err == nil || !strings.Contains(err.Error(), "time.Sleep") {
		t.Fatalf("time.Sleep must stay banned everywhere (use flow.Sleep), got %v", err)
	}
}

func TestLintForbidsOsGetenvInBody(t *testing.T) {
	t.Parallel()
	src := `package main

import "os"

func Run() {
	_ = os.Getenv("FEATURE_FLAG")
}
`
	if err := lintWorkflow(src); err == nil || !strings.Contains(err.Error(), "os.Getenv") {
		t.Fatalf("got %v, want os.Getenv error", err)
	}
}

func TestLintAllowsOsGetenvInsideClosure(t *testing.T) {
	t.Parallel()
	src := `package main

import (
	"context"
	"os"
)

func Run(ctx context.Context) error {
	_ = func(_ context.Context) string {
		return os.Getenv("FEATURE_FLAG")
	}
	return nil
}
`
	if err := lintWorkflow(src); err != nil {
		t.Fatalf("os.Getenv inside closure must be allowed (Step caches output), got %v", err)
	}
}

func TestLintForbidsRandIntnInBody(t *testing.T) {
	t.Parallel()
	// math/rand import is already banned at the import gate; this
	// pins that even if someone aliases the import (e.g. via dot-import
	// or a local rand package), the call-site catches it.
	src := `package main

import "math/rand"

func Run() {
	_ = rand.Intn(10)
}
`
	if err := lintWorkflow(src); err == nil {
		t.Fatal("want error for math/rand import + rand.Intn call")
	}
}

func TestLintForbidsUUIDNewInBody(t *testing.T) {
	t.Parallel()
	src := `package main

import "github.com/google/uuid"

func Run() {
	_ = uuid.New()
}
`
	if err := lintWorkflow(src); err == nil || !strings.Contains(err.Error(), "uuid.New") {
		t.Fatalf("got %v, want uuid.New error", err)
	}
}

func TestLintForbidsMathRand(t *testing.T) {
	t.Parallel()
	src := `package main

import "math/rand"

func main() {
	_ = rand.Intn(10)
}
`
	if err := lintWorkflow(src); err == nil {
		t.Fatal("want error for math/rand import")
	}
}

func TestLintForbidsPanic(t *testing.T) {
	t.Parallel()
	src := `package main

func main() {
	panic("nope")
}
`
	if err := lintWorkflow(src); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("got %v, want panic error", err)
	}
}

func TestLintForbidsEmDashInComments(t *testing.T) {
	t.Parallel()
	dash := "\u2014" // U+2014 EM DASH; escape so source bytes stay clean
	src := fmt.Sprintf(`package main

// Some comment %s nope
func main() {}
`, dash)
	if err := lintWorkflow(src); err == nil || !strings.Contains(err.Error(), "em dash") {
		t.Fatalf("got %v, want em-dash error", err)
	}
}

func TestLintAllowsCleanWorkflow(t *testing.T) {
	t.Parallel()
	src := `package main

import (
	"context"
	"log/slog"
)

func main() {
	slog.Info("hello")
	_ = context.Background()
}
`
	if err := lintWorkflow(src); err != nil {
		t.Fatalf("clean workflow rejected: %v", err)
	}
}
