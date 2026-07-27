package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// dlqFixture builds a journal with one workflow, one run parked in failed_dlq,
// and the dead_letter row the dashboard's Retry button targets.
func dlqFixture(t *testing.T) (*Dispatcher, *journal.Journal, string) {
	t.Helper()
	ctx := context.Background()
	j := newJournal(t)

	if err := j.CreateWorkflow(ctx, "wf_1", "billing", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateRun(ctx, "run_1", "wf_1", "webhook", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.MoveStepToDeadLetter(ctx, "run_1", "charge", "boom", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkRunFinished(ctx, "run_1", "failed_dlq"); err != nil {
		t.Fatal(err)
	}
	item, err := j.FindDeadLetterByRun(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}

	d := &Dispatcher{
		Journal:  j,
		Resolver: &fakeResolver{idByslug: map[string]string{"billing": "wf_1"}},
		// A path that cannot exist: every test here must refuse BEFORE it would
		// spawn anything, so reaching the spawn is itself the failure signal.
		BinaryPath: func(string) (string, error) { return "/nonexistent/workflow", nil },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return d, j, item.ID
}

// TestRetryDeadLetterRefusesDisabledWorkflow pins the kill-switch bypass.
//
// RetryDeadLetter called sup.Run directly instead of going through dispatch(),
// so it skipped every gate that path applies. The one with teeth is the enabled
// flag: an operator disables a misbehaving workflow to stop it running, and the
// Retry button ran it anyway.
func TestRetryDeadLetterRefusesDisabledWorkflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, j, dlqID := dlqFixture(t)

	if err := j.SetWorkflowEnabled(ctx, "wf_1", false); err != nil {
		t.Fatal(err)
	}

	_, err := d.RetryDeadLetter(ctx, dlqID)
	if !errors.Is(err, ErrWorkflowDisabled) {
		t.Fatalf("RetryDeadLetter on a DISABLED workflow returned err=%v, want ErrWorkflowDisabled; disabling a workflow must stop the retry path too", err)
	}
	// And it must not have moved the run out of its terminal state.
	if info, gErr := j.GetRun(ctx, "run_1"); gErr == nil && info.Status == "running" {
		t.Fatal("refused retry still flipped the run to running")
	}
}

// TestRetryDeadLetterRespectsCapacity pins the unbounded-fork gap: the retry
// path took no concurrency slot, so MaxConcurrent did not apply to it and an
// operator retrying a backlog of dead letters could fork past the cap.
func TestRetryDeadLetterRespectsCapacity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, _, dlqID := dlqFixture(t)
	d.MaxConcurrent = 1

	// Occupy the only slot, as an in-flight run would.
	if !d.acquire() {
		t.Fatal("precondition: first acquire should succeed")
	}
	defer d.release()

	if _, err := d.RetryDeadLetter(ctx, dlqID); !errors.Is(err, ErrCapacity) {
		t.Fatalf("RetryDeadLetter at capacity returned err=%v, want ErrCapacity; the retry path must shed load like every other dispatch", err)
	}
}

// TestStartDeadLetterRetryIsSingleFlight pins the double-execution bug: the
// retry used the unguarded SetRunStatus, so two concurrent clicks both flipped
// the run to running and both spawned a supervisor against the SAME run id.
func TestStartDeadLetterRetryIsSingleFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, j, _ := dlqFixture(t)

	first, err := j.StartDeadLetterRetry(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first claim lost; a failed_dlq run must be claimable")
	}
	second, err := j.StartDeadLetterRetry(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("second concurrent claim WON; two supervisors would run against one run id and duplicate its side effects")
	}
}

// TestStartDeadLetterRetryWillNotResurrectACancelledRun keeps the existing
// cancel guard (MarkRunFinished, ResumeSuspendedRun) honest on this path too.
func TestStartDeadLetterRetryWillNotResurrectACancelledRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, j, _ := dlqFixture(t)

	if _, err := j.StartDeadLetterRetry(ctx, "run_1"); err != nil {
		t.Fatal(err)
	}
	if err := j.FinalizeCancel(ctx, "run_1"); err != nil {
		t.Fatal(err)
	}
	claimed, err := j.StartDeadLetterRetry(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("a CANCELLED run was claimed for retry; cancellation must be final on every path")
	}
}
