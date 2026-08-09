package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"text/tabwriter"

	"github.com/bright-interaction/reactor/internal/codegen"
	"github.com/bright-interaction/reactor/internal/migrate"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// cmdWorkflow dispatches the workflow subcommand group.
//
//	reactor workflow list   --db <url>
//	reactor workflow register --db <url> --slug <slug> [--sdk-version <ver>]
//	reactor workflow build  --src <dir> --out <root>/workflows/<slug>/workflow
//
// The build subcommand wraps `go build` so users on a blank install
// don't need to know the convention; it builds <src> into the daemon's
// expected path. register stamps the workflows row.
func cmdWorkflow(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("workflow: missing subcommand (list|register|build)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdWorkflowList(ctx, log, rest)
	case "register":
		return cmdWorkflowRegister(ctx, log, rest)
	case "build":
		return cmdWorkflowBuild(rest)
	default:
		return fmt.Errorf("workflow: unknown subcommand %q (want list|register|build)", sub)
	}
}

func cmdWorkflowList(ctx context.Context, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("workflow list", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("missing --db (or $REACTOR_DB_URL)")
	}

	j, closer, err := openJournalForCLI(*dbURL)
	if err != nil {
		return err
	}
	defer closer()

	wfs, err := j.ListWorkflows(ctx)
	if err != nil {
		return err
	}

	if *asJSON {
		if wfs == nil {
			wfs = []journal.Workflow{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(wfs)
	}
	if len(wfs) == 0 {
		fmt.Println("(no workflows)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "SLUG\tID\tSDK\tUPDATED")
	for _, w := range wfs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", w.Slug, w.ID, w.SDKVersion,
			w.UpdatedAt.UTC().Format("2006-01-02T15:04Z"))
	}
	return nil
}

func cmdWorkflowRegister(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("workflow register", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL")
	slug := fs.String("slug", "", "workflow slug (required)")
	sdkVersion := fs.String("sdk-version", "0.1.0", "SDK version this workflow was authored against")
	dagPath := fs.String("dag", "", "path to dag.json (optional)")
	srcPath := fs.String("src", "", "path to workflow.go (used to compute code hash, optional)")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *dbURL == "" || *slug == "" {
		return errors.New("workflow register: --db and --slug required")
	}
	if !codegen.IsValidSlug(*slug) {
		return fmt.Errorf("workflow register: --slug %q must match ^[a-z][a-z0-9-]*$ (becomes a filesystem path; no traversal allowed)", *slug)
	}

	j, closer, err := openJournalForCLI(*dbURL)
	if err != nil {
		return err
	}
	defer closer()

	// Idempotency: if a workflow with this slug already exists, update
	// the SDK version + code hash + dag rather than inserting a duplicate.
	//
	// Scoped to the tenant CreateWorkflow below actually writes to
	// (DefaultTenant). Slugs are unique per tenant, so the unscoped lookup
	// answered "does ANY tenant own this slug" and another tenant's row made this
	// print "already registered (id=...)" while creating nothing here.
	existing, err := j.WorkflowIDBySlugInTenant(ctx, *slug, journal.DefaultTenant)
	if err != nil && !errors.Is(err, journal.ErrNotFound) {
		return err
	}

	codeHash := ""
	if *srcPath != "" {
		h, err := hashFile(*srcPath)
		if err != nil {
			return err
		}
		codeHash = h
	}
	dag := json.RawMessage(`{}`)
	if *dagPath != "" {
		raw, err := os.ReadFile(*dagPath)
		if err != nil {
			return fmt.Errorf("read dag: %w", err)
		}
		if !json.Valid(raw) {
			return errors.New("dag: invalid json")
		}
		dag = raw
	}

	if existing != "" {
		log.Info("workflow register: slug already present, no-op (re-registration semantics land alongside the codegen MCP tool)",
			"slug", *slug, "id", existing)
		fmt.Printf("workflow %s already registered (id=%s)\n", *slug, existing)
		return nil
	}

	id, err := newWorkflowID()
	if err != nil {
		return err
	}
	if err := j.CreateWorkflow(ctx, id, *slug, codeHash, *sdkVersion, dag); err != nil {
		return err
	}
	fmt.Printf("registered %s as %s\n", *slug, id)
	return nil
}

func cmdWorkflowBuild(args []string) error {
	fs := flag.NewFlagSet("workflow build", flag.ContinueOnError)
	src := fs.String("src", "", "path to workflow source directory (must contain main.go)")
	root := fs.String("root", defaultRoot(), "Reactor state root (binaries land in <root>/workflows/<slug>/workflow)")
	slug := fs.String("slug", "", "workflow slug (required; the binary lands at <root>/workflows/<slug>/workflow)")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *src == "" || *slug == "" {
		return errors.New("workflow build: --src and --slug required")
	}
	if !codegen.IsValidSlug(*slug) {
		return fmt.Errorf("workflow build: --slug %q must match ^[a-z][a-z0-9-]*$ (becomes a filesystem path; no traversal allowed)", *slug)
	}
	if *root == "" {
		return errors.New("workflow build: --root required (and HOME unset)")
	}

	out := filepath.Join(*root, "workflows", *slug, "workflow")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return fmt.Errorf("workflow build: mkdir: %w", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("workflow build: the Go toolchain is required to compile a workflow but %q was not found on PATH: %w", "go", err)
	}
	// Parity with the daemon's codegen/upload build path: enforce the import
	// allowlist (the build-time RCE gate - blocks import "C"/os/exec/third-party
	// modules) and the lint pass BEFORE compiling. Without this, `reactor
	// workflow build` was the one authoring surface that skipped the gate.
	if err := codegen.CheckAllowedImports(*src); err != nil {
		return fmt.Errorf("workflow build: %w", err)
	}
	if issues, lErr := codegen.LintDir(*src); lErr != nil {
		return fmt.Errorf("workflow build: lint: %w", lErr)
	} else if len(issues) > 0 {
		for _, is := range issues {
			fmt.Fprintf(os.Stderr, "lint: %s:%d:%d [%s] %s\n", is.Path, is.Line, is.Col, is.Rule, is.Message)
		}
		return fmt.Errorf("workflow build: %d lint issue(s); fix them before building", len(issues))
	}
	cmd := exec.Command("go", "build", codegen.BuildVCSFlag, "-o", out, ".")
	cmd.Dir = *src
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// Strip REACTOR_*/ARACHNE_*/ANTHROPIC_API_KEY from the build environment: a
	// `go build` has no business reading the vault master key or DB URL, and a
	// hostile module dependency could otherwise exfiltrate them. Matches the
	// daemon's codegen/upload build path (codegen.BuildAndRegister).
	cmd.Env = codegen.SecureBuildEnv()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("workflow build: go build: %w", err)
	}
	fmt.Printf("built %s -> %s\n", *src, out)
	return nil
}

func openJournalForCLI(dbURL string) (*journal.Journal, func(), error) {
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

func newWorkflowID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "wf_" + hex.EncodeToString(b), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
