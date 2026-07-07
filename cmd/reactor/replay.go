package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/brightinteraction/reactor/internal/runtime/journal"
)

// cmdReplay walks the journal for a given run-id and prints the recorded
// timeline (one row per step attempt) plus the final run status. This is
// the read-only surface week 7 ships; the full re-execute-with-supervisor
// flow uses the same Mode="replay" guard already in place on the
// supervisor and lands in week 8 alongside the dashboard.
//
// Refuses runs in `running` status because their journal is still being
// written; replay only makes sense against a finalised or suspended run.
func cmdReplay(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("missing --db (or $REACTOR_DB_URL)")
	}
	if fs.NArg() == 0 {
		return errors.New("missing <run-id>")
	}
	runID := fs.Arg(0)
	log.Debug("replay", "run_id", runID, "db", redactDB(*dbURL))

	j, closer, err := openJournal(*dbURL)
	if err != nil {
		return err
	}
	defer closer()

	info, err := j.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			return fmt.Errorf("replay: run %s not found", runID)
		}
		return err
	}
	if info.Status == "running" {
		return fmt.Errorf("replay: run %s is still running; replay only supports finalised or suspended runs", runID)
	}

	steps, err := j.ListSteps(ctx, runID)
	if err != nil {
		return err
	}

	fmt.Printf("Run:        %s\n", info.ID)
	fmt.Printf("Workflow:   %s\n", info.WorkflowID)
	fmt.Printf("Trigger:    %s\n", info.TriggerKind)
	fmt.Printf("Status:     %s\n", info.Status)
	if !info.StartedAt.IsZero() {
		fmt.Printf("Started:    %s\n", info.StartedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if !info.FinishedAt.IsZero() {
		fmt.Printf("Finished:   %s\n", info.FinishedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	fmt.Println()

	if len(steps) == 0 {
		fmt.Println("(no recorded steps)")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "STEP\tATTEMPT\tSTATUS\tDURATION_MS\tERROR")
	for _, s := range steps {
		dur := ""
		if !s.StartedAt.IsZero() && !s.FinishedAt.IsZero() {
			dur = fmt.Sprintf("%d", s.FinishedAt.Sub(s.StartedAt).Milliseconds())
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n",
			s.StepName, s.Attempt, s.Status, dur, truncate(s.ErrorText, 80),
		)
	}
	return nil
}
