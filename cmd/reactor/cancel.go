package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"

	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// cmdCancel requests cancellation of a run. Works from anywhere with DB
// access: a suspended run is cancelled outright; a running run is flagged
// and the daemon's cancel watcher kills its subprocess within a couple of
// seconds. A terminal run is reported as nothing-to-do.
//
//	reactor cancel <run-id> --db <url>
func cmdCancel(ctx context.Context, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("cancel: missing <run-id>")
	}
	runID := fs.Arg(0)
	if *dbURL == "" {
		return errors.New("cancel: missing --db (or $REACTOR_DB_URL)")
	}

	j, closeJ, err := openJournal(*dbURL)
	if err != nil {
		return err
	}
	defer closeJ()

	outcome, err := j.RequestRunCancel(ctx, runID)
	if err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			return fmt.Errorf("cancel: run %q not found", runID)
		}
		return err
	}
	switch outcome {
	case journal.CancelDone:
		fmt.Printf("cancelled %s (was suspended; will not resume)\n", runID)
	case journal.CancelRequested:
		fmt.Printf("cancellation requested for %s (running; the daemon will stop it within ~2s)\n", runID)
	default:
		fmt.Printf("run %s is already finished; nothing to cancel\n", runID)
	}
	return nil
}
