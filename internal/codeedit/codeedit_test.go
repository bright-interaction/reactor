package codeedit

import (
	"strings"
	"testing"
)

const sampleSrc = `package main

import (
	"context"

	"reactor"
)

type Payload struct{ ID string }

func Run(ctx context.Context, flow reactor.Flow, in Payload) error {
	startUnix, err := reactor.SideEffect(flow, ctx, "captured-time", func() int64 {
		return 1700000000
	})
	if err != nil {
		return err
	}

	customer, err := reactor.Step(flow, ctx, "fetch-customer", reactor.StepOpts{
		TimeoutSeconds: 30,
	}, func(ctx context.Context) (string, error) {
		return "ada@example.com", nil
	})
	if err != nil {
		return err
	}

	if _, err := reactor.Step(flow, ctx, "send-welcome", reactor.StepOpts{
		IdempotencyKey: customer,
	}, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	_ = startUnix
	return nil
}
`

func TestStepNames(t *testing.T) {
	got, err := StepNames(sampleSrc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"captured-time", "fetch-customer", "send-welcome"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("StepNames = %v, want %v", got, want)
	}
}

func TestExtractStep_Assign(t *testing.T) {
	snippet, err := ExtractStep(sampleSrc, "fetch-customer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snippet, "customer, err := reactor.Step(flow, ctx, \"fetch-customer\"") {
		t.Fatalf("snippet has unexpected start:\n%s", snippet)
	}
	if !strings.HasSuffix(strings.TrimSpace(snippet), "})") {
		t.Fatalf("snippet has unexpected end:\n%s", snippet)
	}
	if strings.Contains(snippet, "send-welcome") {
		t.Fatalf("snippet leaked into the next step:\n%s", snippet)
	}
}

func TestExtractStep_IfStmt(t *testing.T) {
	snippet, err := ExtractStep(sampleSrc, "send-welcome")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snippet, "if _, err := reactor.Step(flow, ctx, \"send-welcome\"") {
		t.Fatalf("expected the whole if-statement, got:\n%s", snippet)
	}
	if !strings.HasSuffix(strings.TrimSpace(snippet), "}") {
		t.Fatalf("if-statement not closed:\n%s", snippet)
	}
}

func TestReplaceStep_RoundTrips(t *testing.T) {
	orig, err := ExtractStep(sampleSrc, "fetch-customer")
	if err != nil {
		t.Fatal(err)
	}
	out, err := ReplaceStep(sampleSrc, "fetch-customer", orig)
	if err != nil {
		t.Fatal(err)
	}
	if out != sampleSrc {
		t.Fatalf("replacing a step with itself changed the source")
	}
}

func TestReplaceStep_Edits(t *testing.T) {
	edited := `customer, err := reactor.Step(flow, ctx, "fetch-customer", reactor.StepOpts{
		TimeoutSeconds: 90,
	}, func(ctx context.Context) (string, error) {
		return "grace@example.com", nil
	})`
	out, err := ReplaceStep(sampleSrc, "fetch-customer", edited)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TimeoutSeconds: 90") || !strings.Contains(out, "grace@example.com") {
		t.Fatalf("edit not applied:\n%s", out)
	}
	if strings.Contains(out, "TimeoutSeconds: 30") || strings.Contains(out, "ada@example.com") {
		t.Fatalf("old code not removed:\n%s", out)
	}
	// Other steps must be untouched.
	if !strings.Contains(out, "captured-time") || !strings.Contains(out, "send-welcome") {
		t.Fatalf("neighbouring steps were damaged:\n%s", out)
	}
}

func TestNotFound(t *testing.T) {
	_, err := ExtractStep(sampleSrc, "no-such-step")
	if _, ok := err.(*ErrNotFound); !ok {
		t.Fatalf("want ErrNotFound, got %T: %v", err, err)
	}
}
