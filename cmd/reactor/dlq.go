package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/brightinteraction/reactor/internal/migrate"
	"github.com/brightinteraction/reactor/internal/runtime/journal"
	"github.com/brightinteraction/reactor/internal/runtime/supervisor"
)

// cmdDLQ dispatches the dlq subcommand group: list / show / retry.
//
// retry re-runs the workflow with the same RunID. The supervisor's
// journal cache short-circuits previously-succeeded steps; the failed
// step has no succeeded output_jsonb row so it re-executes. On run
// success, the dead_letter row is deleted; on failure, last_rotation_
// error semantics apply (run terminal flips to failed_dlq again).
func cmdDLQ(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("dlq: missing subcommand (list|show|retry)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdDLQList(ctx, log, rest)
	case "show":
		return cmdDLQShow(ctx, log, rest)
	case "retry":
		return cmdDLQRetry(ctx, log, rest)
	default:
		return fmt.Errorf("dlq: unknown subcommand %q (want list|show|retry)", sub)
	}
}

func cmdDLQList(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("dlq list", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL")
	limit := fs.Int("limit", 50, "max items to return")
	offset := fs.Int("offset", 0, "rows to skip")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("missing --db (or $REACTOR_DB_URL)")
	}

	j, closer, err := openJournal(*dbURL)
	if err != nil {
		return err
	}
	defer closer()

	items, err := j.ListDeadLetterItems(ctx, *limit, *offset)
	if err != nil {
		return err
	}
	log.Debug("dlq list", "count", len(items), "db", redactDB(*dbURL))

	if *asJSON {
		// JSON consumers expect an array; a nil slice would marshal to
		// `null` and break `.length` / `.forEach` callers. Initialise
		// to an empty slice so the on-wire shape is always `[]` or `[ ... ]`.
		if items == nil {
			items = []journal.DeadLetterItem{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}
	if len(items) == 0 {
		fmt.Println("(no dead-letter items)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "ID\tRUN_ID\tSTEP\tMOVED_AT\tERROR")
	for _, it := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			it.ID, it.RunID, it.StepName,
			it.MovedAt.UTC().Format("2006-01-02T15:04:05Z"),
			truncate(it.ErrorText, 80),
		)
	}
	return nil
}

func cmdDLQShow(ctx context.Context, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("dlq show", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL")
	asJSON := fs.Bool("json", false, "emit JSON")
	// stdlib flag.Parse stops at the first non-flag arg. Users naturally
	// type `reactor dlq show --db ... <id> --json` (positional in the
	// middle), so reorder args so flags lead and positionals trail.
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("missing --db (or $REACTOR_DB_URL)")
	}
	if fs.NArg() == 0 {
		return errors.New("missing <id>")
	}
	id := fs.Arg(0)

	j, closer, err := openJournal(*dbURL)
	if err != nil {
		return err
	}
	defer closer()

	item, err := j.GetDeadLetterItem(ctx, id)
	if err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			return fmt.Errorf("dlq: %s not found", id)
		}
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(item)
	}
	fmt.Printf("ID:        %s\n", item.ID)
	fmt.Printf("Run:       %s\n", item.RunID)
	fmt.Printf("Step:      %s\n", item.StepName)
	fmt.Printf("Moved at:  %s\n", item.MovedAt.UTC().Format("2006-01-02T15:04:05Z"))
	fmt.Printf("Error:     %s\n", item.ErrorText)
	fmt.Printf("Payload:   %s\n", string(item.Payload))
	return nil
}

// cmdDLQRetry re-runs the workflow that owns the dead-letter row by
// spawning a fresh supervisor with the original RunID. The supervisor's
// journal cache short-circuits every previously-succeeded step; the
// failed step has no succeeded output_jsonb row so the closure runs
// again. On success, the dead_letter row is removed.
//
//	reactor dlq retry --db <url> --master-key-file <path> <dlq-id>
//
// Requires the workflow binary registry root because re-running needs
// to look up the binary path.
func cmdDLQRetry(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("dlq retry", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL")
	root := fs.String("root", defaultRoot(), "Reactor state root (workflows/ + master.key live here)")
	masterKeyFile := fs.String("master-key-file", "", "path to a 64-hex-char master key (default <root>/master.key)")
	masterKeyHex := fs.String("master-key", envFirst("REACTOR_MASTER_KEY", "ARACHNE_MASTER_KEY"), "32-byte hex master key (overrides --master-key-file)")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("dlq retry: missing --db (or $REACTOR_DB_URL)")
	}
	if fs.NArg() == 0 {
		return errors.New("dlq retry: missing <dlq-id>")
	}
	dlqID := fs.Arg(0)
	if *root == "" {
		return errors.New("dlq retry: missing --root (and HOME unset)")
	}
	masterHex, err := loadMasterKey(*masterKeyHex, *masterKeyFile, *root)
	if err != nil {
		return err
	}

	j, closer, err := openJournal(*dbURL)
	if err != nil {
		return err
	}
	defer closer()

	item, err := j.GetDeadLetterItem(ctx, dlqID)
	if err != nil {
		if errors.Is(err, journal.ErrNotFound) {
			return fmt.Errorf("dlq retry: %s not found", dlqID)
		}
		return err
	}

	// Open vault + binary registry the same way `serve` does so retries
	// run against the same workflow binary the daemon would use.
	store, vaultCloser, err := openVaultStore(*dbURL, masterHex)
	if err != nil {
		return err
	}
	defer vaultCloser()

	run, err := j.GetRun(ctx, item.RunID)
	if err != nil {
		return fmt.Errorf("dlq retry: get run: %w", err)
	}
	slug, err := j.WorkflowSlugByID(ctx, run.WorkflowID)
	if err != nil {
		return fmt.Errorf("dlq retry: workflow lookup: %w", err)
	}
	binaryPath, err := workflowBinaryPath(*root, slug)
	if err != nil {
		return err
	}

	// Restore the run to "running" so the supervisor's terminal-status
	// flip writes the right value. The dead_letter row stays until the
	// retry actually succeeds.
	if err := j.SetRunStatus(ctx, item.RunID, "running"); err != nil {
		return fmt.Errorf("dlq retry: reset run status: %w", err)
	}

	signKey, _ := decodeMasterKey(masterHex)
	sup := supervisor.Supervisor{
		BinaryPath:       binaryPath,
		WorkflowSlug:     slug,
		RunID:            item.RunID,
		Mode:             "live",
		Journal:          j,
		Vault:            store,
		Log:              log,
		Input:            item.Payload,
		SignalSigningKey: signKey,
	}
	status, err := sup.Run(ctx)
	if err != nil {
		return fmt.Errorf("dlq retry: %w", err)
	}
	fmt.Printf("retry %s -> run %s status=%s\n", dlqID, item.RunID, status)
	if status == "succeeded" {
		if err := j.DeleteDeadLetter(ctx, dlqID); err != nil && !errors.Is(err, journal.ErrNotFound) {
			log.Warn("dlq retry: cleanup failed", "err", err)
		} else {
			fmt.Printf("dead-letter %s cleared\n", dlqID)
		}
	}
	return nil
}

// workflowBinaryPath resolves <root>/workflows/<slug>/workflow and
// confirms the binary is executable. Mirrors registry.FileRegistry but
// kept inline so dlq retry doesn't need to import the registry package.
func workflowBinaryPath(root, slug string) (string, error) {
	if slug == "" {
		return "", errors.New("dlq retry: empty slug")
	}
	path := filepath.Join(root, "workflows", slug, "workflow")
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("dlq retry: stat %s: %w (run `reactor workflow build`?)", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("dlq retry: %s is a directory", path)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("dlq retry: %s is not executable", path)
	}
	return path, nil
}

// openJournal centralises the migrate.Open + journal.New wiring used by
// every subcommand that needs to read the schedules / runs / dead_letter
// tables. Returns a closer that callers must defer.
func openJournal(dbURL string) (*journal.Journal, func(), error) {
	db, engine, err := migrate.Open(dbURL)
	if err != nil {
		return nil, nil, err
	}
	closer := func() { _ = db.Close() }
	jEngine := journal.EngineSQLite
	if engine == migrate.EnginePostgres {
		jEngine = journal.EnginePostgres
	}
	return journal.New(db, jEngine), closer, nil
}

// truncate trims s to n runes plus an ellipsis when it overflows. Used
// for the list command's error column so a multi-line stack trace doesn't
// blow out the table.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// boolFlags are the bool-shaped flags every CLI subcommand recognises.
// reorderArgs hoists these from anywhere in the argv to the front so a
// user can write the natural `dlq show --db <url> <id> --json` instead
// of being forced to put the bool flag before the positional.
var boolFlags = map[string]bool{
	"--json":         true,
	"-json":          true,
	"--latest":       true,
	"-latest":        true,
	"--echo-prompt":  true,
	"-echo-prompt":   true,
	"--allow-write":  true,
	"-allow-write":   true,
	"--apply":        true,
	"-apply":         true,
	"--gold":         true,
	"-gold":          true,
	"--off":          true,
	"-off":           true,
	"--no-commit":    true,
	"-no-commit":     true,
	"--auto-rotate":  true,
	"-auto-rotate":   true,
	"-h":             true,
	"--help":         true,
}

// reorderArgs hoists `--name=value` tokens and known bool flags ahead
// of positional tokens. `--name value` (string flag with following
// value) is left in place; users still need to put those before the
// first positional, which matches the stdlib `flag` package's contract.
//
// Anything past `--` is treated as positional verbatim.
func reorderArgs(args []string) []string {
	flags := []string{}
	pos := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			return append(flags, pos...)
		case strings.Contains(a, "=") && strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		case boolFlags[a]:
			flags = append(flags, a)
		default:
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}
