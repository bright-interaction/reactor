package postmortem

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/brightinteraction/reactor/internal/codegen"
	"github.com/brightinteraction/reactor/internal/knowledge"
	"github.com/brightinteraction/reactor/internal/migrate"
	"github.com/brightinteraction/reactor/internal/runtime/journal"
)

// fakeAnthropic returns the canned Postmortem regardless of input.
type fakeAnthropic struct {
	pm        Postmortem
	failFirst int // return error N times before succeeding
	calls     int
	failErr   error
}

func (f *fakeAnthropic) SendMessages(_ context.Context, _ codegen.MessagesRequest) (*codegen.MessagesResponse, error) {
	f.calls++
	if f.failFirst > 0 {
		f.failFirst--
		err := f.failErr
		if err == nil {
			err = errors.New("simulated transient failure")
		}
		return nil, err
	}
	body, err := json.Marshal(f.pm)
	if err != nil {
		return nil, err
	}
	return &codegen.MessagesResponse{
		Content: []codegen.ContentBlock{{
			Type:  "tool_use",
			ID:    "toolu_test",
			Name:  postmortemToolName,
			Input: body,
		}},
	}, nil
}

// freshJournalAndKnowledge builds a sqlite-backed Journal with
// migrations applied (via migrate.Up so goose's package globals stay
// behind the gooseMu lock + parallel tests are race-clean), plus a
// temp Knowledge store at <dir>/knowledge.
func freshJournalAndKnowledge(t *testing.T) (*journal.Journal, *knowledge.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dsn := "sqlite://" + dir + "/reactor.db"
	if err := migrate.Up(context.Background(), slog.Default(), dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn, _, err := migrate.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	j := journal.New(conn, journal.EngineSQLite)
	store, err := knowledge.New(dir + "/knowledge")
	if err != nil {
		t.Fatalf("knowledge: %v", err)
	}
	return j, store, func() { conn.Close() }
}

// seedFailedRun inserts a workflow + run + dead-letter row so Generate
// has something to chew on.
func seedFailedRun(t *testing.T, j *journal.Journal) (runID string) {
	t.Helper()
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_test", "test-workflow", "", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	runID = "run_test_dlq"
	if err := j.CreateRun(ctx, runID, "wf_test", "manual", json.RawMessage(`{"hello":"world"}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.MoveStepToDeadLetter(ctx, runID, "boom-step", "permanent error from upstream", json.RawMessage(`{"hello":"world"}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkRunFinished(ctx, runID, "failed_dlq"); err != nil {
		t.Fatal(err)
	}
	return runID
}

func TestGenerateWritesPostmortemToCorpus(t *testing.T) {
	t.Parallel()
	j, store, closer := freshJournalAndKnowledge(t)
	defer closer()

	runID := seedFailedRun(t, j)

	g := &Generator{
		Anthropic: &fakeAnthropic{pm: Postmortem{
			Title:          "Don't fire-and-forget when the upstream rejects with 4xx",
			Summary:        "The boom-step posted to a downstream that returned 400 and the closure did not wrap it Permanent.",
			RootCause:      "Missing reactor.Permanent on a 4xx branch caused infinite retry until DLQ.",
			Lesson:         "Wrap 4xx errors with reactor.Permanent so the supervisor stops retrying immediately.",
			Recommendation: "Add a status-code switch around the response; Permanent for 4xx (except 429), Retryable for 5xx + 429.",
			Tags:           []string{"errors", "retry"},
		}},
		Journal:   j,
		Knowledge: store,
	}
	id, err := g.Generate(context.Background(), runID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	entry, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("entry not persisted: %v", err)
	}
	if entry.Frontmatter.Topic != "post-mortems" {
		t.Errorf("topic = %q, want post-mortems", entry.Frontmatter.Topic)
	}
	if entry.Frontmatter.CreatedBy != "claude" {
		t.Errorf("created_by = %q, want claude", entry.Frontmatter.CreatedBy)
	}
	gotRunRef := false
	for _, src := range entry.Frontmatter.Sources {
		if src == "run:"+runID {
			gotRunRef = true
		}
	}
	if !gotRunRef {
		t.Errorf("sources missing run reference; got %v", entry.Frontmatter.Sources)
	}
	if !strings.Contains(entry.Body, "Wrap 4xx errors with reactor.Permanent") {
		t.Errorf("lesson body missing from rendered entry")
	}
}

func TestGenerateSkipsWhenAnthropicMissing(t *testing.T) {
	t.Parallel()
	j, store, closer := freshJournalAndKnowledge(t)
	defer closer()
	g := &Generator{Journal: j, Knowledge: store} // Anthropic nil
	_, err := g.Generate(context.Background(), "anything")
	if !errors.Is(err, ErrAnthropicMissing) {
		t.Fatalf("got %v, want ErrAnthropicMissing", err)
	}
}

func TestGenerateBlocksPIIInLesson(t *testing.T) {
	t.Parallel()
	j, store, closer := freshJournalAndKnowledge(t)
	defer closer()
	runID := seedFailedRun(t, j)

	// Claude tries to write a lesson that mentions a customer email.
	// The knowledge.Store redactor should reject it and Generate should
	// surface the redaction error instead of a silent persist.
	g := &Generator{
		Anthropic: &fakeAnthropic{pm: Postmortem{
			Title:          "Title is fine",
			Lesson:         "User customer@example.com triggered the 4xx; the body included tom@example.org too.",
			Recommendation: "fix it",
		}},
		Journal:   j,
		Knowledge: store,
	}
	_, err := g.Generate(context.Background(), runID)
	if err == nil {
		t.Fatal("expected redactor to block PII-bearing post-mortem")
	}
	var red *knowledge.ErrRedacted
	if !errors.As(err, &red) {
		t.Fatalf("expected ErrRedacted, got %T: %v", err, err)
	}
}

func TestGenerateReturnsErrorOnEmptyToolUse(t *testing.T) {
	t.Parallel()
	j, store, closer := freshJournalAndKnowledge(t)
	defer closer()
	runID := seedFailedRun(t, j)

	// Title and Lesson empty -> extractPostmortem must reject.
	g := &Generator{
		Anthropic: &fakeAnthropic{pm: Postmortem{}},
		Journal:   j,
		Knowledge: store,
	}
	_, err := g.Generate(context.Background(), runID)
	if err == nil {
		t.Fatal("expected error on empty postmortem")
	}
}
