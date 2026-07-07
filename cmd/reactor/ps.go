package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
)

// cmdPS reports daemon-state-of-the-world: workflow + run + credential
// counts, recent activity, last rotation. Read-only against the
// database; no daemon ping required so an operator can run `reactor
// ps --db ...` from anywhere with DB access. Pairs with /healthz when
// the daemon happens to be reachable but isn't a substitute for it.
//
//	reactor ps --db <url> [--json]
func cmdPS(ctx context.Context, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("ps: missing --db (or $REACTOR_DB_URL)")
	}

	j, jClose, err := openJournal(*dbURL)
	if err != nil {
		return err
	}
	defer jClose()
	credRepo, cClose, err := openCredentials(*dbURL)
	if err != nil {
		return err
	}
	defer cClose()

	wfs, err := j.ListWorkflows(ctx)
	if err != nil {
		return err
	}
	runs, err := j.ListRecentRuns(ctx, 100)
	if err != nil {
		return err
	}
	creds, err := credRepo.List(ctx)
	if err != nil {
		return err
	}
	dlqItems, err := j.ListDeadLetterItems(ctx, 1000, 0)
	if err != nil {
		return err
	}

	statusCounts := map[string]int{}
	for _, r := range runs {
		statusCounts[r.Status]++
	}
	autoRotate := 0
	rotationErrors := 0
	for _, c := range creds {
		if c.AutoRotate {
			autoRotate++
		}
		if c.LastRotationError != "" {
			rotationErrors++
		}
	}

	report := map[string]any{
		"workflows":       len(wfs),
		"runs_recent":     len(runs),
		"runs_by_status":  statusCounts,
		"credentials":     len(creds),
		"auto_rotate":     autoRotate,
		"rotation_errors": rotationErrors,
		"dead_letter":     len(dlqItems),
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "Workflows registered:\t", len(wfs))
	fmt.Fprintln(tw, "Recent runs (last 100):\t", len(runs))
	for _, status := range []string{"running", "succeeded", "failed", "failed_dlq", "suspended"} {
		if n := statusCounts[status]; n > 0 {
			fmt.Fprintf(tw, "  %s\t%d\n", status, n)
		}
	}
	fmt.Fprintln(tw, "Credentials:\t", len(creds))
	fmt.Fprintln(tw, "  with auto-rotate:\t", autoRotate)
	if rotationErrors > 0 {
		fmt.Fprintf(tw, "  with rotation errors:\t%d\n", rotationErrors)
	}
	fmt.Fprintln(tw, "Dead-letter items:\t", len(dlqItems))
	return nil
}
