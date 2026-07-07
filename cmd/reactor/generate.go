package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/brightinteraction/reactor/internal/codegen"
	"github.com/brightinteraction/reactor/internal/graph"
	"github.com/brightinteraction/reactor/internal/knowledge"
)

// cmdGenerate runs the codegen orchestrator from the CLI.
//
//	reactor generate "brief in natural language"
//	reactor generate --brief-file path/to/brief.md
//	reactor generate --workflows-dir <dir> --no-commit "brief"
//
// The orchestrator: builds the prompt, calls the Anthropic Messages
// API with tool-forced JSON output (the AnthropicClient + EmitToolName
// schema enforce the {slug, version, workflow_go, dag_json,
// workflow_test_go} shape), writes the four files into a temp dir,
// runs the validator chain (go vet + reactor lint + go build), retries
// the LLM up to --max-retries times on validation failure with the
// errors fed back into the conversation, atomically renames into
// reactor-workflows/<slug>/, and commits via the configured git
// Committer.
//
// Requires ANTHROPIC_API_KEY in the environment. The codegen package's
// NewAnthropicFromEnv reads it.
func cmdGenerate(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	briefFile := fs.String("brief-file", "", "path to a file containing the workflow brief (overrides positional + --brief)")
	briefFlag := fs.String("brief", "", "the workflow brief (overrides positional)")
	envContext := fs.String("environment", "", "optional environment context (services, credential IDs, schemas) the model can reference")
	workflowsDir := fs.String("workflows-dir", "reactor-workflows", "where the generated workflow directory lands")
	noCommit := fs.Bool("no-commit", false, "skip the git commit step (useful for sandbox runs)")
	maxRetries := fs.Int("max-retries", 3, "validation-retry budget; the model gets the lint/build errors fed back each round")
	root := fs.String("root", defaultRoot(), "Reactor state root (knowledge corpus + graph; lens disabled if empty)")
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL (lens graph queries the journal; lens disabled if empty)")
	echoPrompt := fs.Bool("echo-prompt", false, "print the assembled user message to stderr before sending; useful for verifying lens injection")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}

	brief, err := resolveBrief(*briefFlag, *briefFile, fs.Args())
	if err != nil {
		return err
	}

	client, err := codegen.NewAnthropicFromEnv()
	if err != nil {
		return fmt.Errorf("generate: %w (set ANTHROPIC_API_KEY)", err)
	}

	gen := &codegen.Generator{
		Anthropic:    client,
		Log:          log,
		WorkflowsDir: *workflowsDir,
		MaxRetries:   *maxRetries,
	}
	if *noCommit {
		gen.Committer = noopCommitter{}
	}

	// Build the Environment Lens. Both inputs are optional; the lens
	// adapter no-ops the missing leg so a fresh install with no DB
	// can still inject knowledge entries, and a stateful install with
	// no knowledge dir can still inject the graph slice.
	lens, lensCleanup, err := buildLens(*root, *dbURL, log)
	if err != nil {
		return err
	}
	defer lensCleanup()
	gen.Lens = lens

	if *echoPrompt {
		gen.EchoPrompt = func(p string) {
			fmt.Fprintln(os.Stderr, "----- assembled user message -----")
			fmt.Fprintln(os.Stderr, p)
			fmt.Fprintln(os.Stderr, "----- end assembled user message -----")
		}
	}

	log.Info("generate: starting", "workflows_dir", *workflowsDir, "max_retries", *maxRetries)
	result, err := gen.Generate(ctx, codegen.GenerateRequest{
		Brief:       brief,
		Environment: *envContext,
	})
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	fmt.Printf("generated %s v%s -> %s (attempts=%d, tokens=%d/%d in/out)\n",
		result.Slug, result.Version, result.Path, result.Attempts,
		result.UsageInput, result.UsageOutput)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  reactor workflow build --src %s --slug %s --root <root>\n", result.Path, result.Slug)
	fmt.Printf("  reactor workflow register --db <url> --slug %s --src %s/main.go\n", result.Slug, result.Path)
	return nil
}

// resolveBrief picks the brief from (in priority order) --brief-file,
// --brief, or the positional argument. Returns an error when none is
// provided + no brief is on stdin.
func resolveBrief(flagBrief, file string, positional []string) (string, error) {
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read brief file: %w", err)
		}
		return string(raw), nil
	}
	if flagBrief != "" {
		return flagBrief, nil
	}
	if len(positional) > 0 {
		// Join multiple positional args so an unquoted brief still works.
		out := positional[0]
		for _, p := range positional[1:] {
			out += " " + p
		}
		return out, nil
	}
	// stdin fallback: handy for piping a long brief from a file.
	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		raw, err := io.ReadAll(os.Stdin)
		if err == nil && len(raw) > 0 {
			return string(raw), nil
		}
	}
	return "", errors.New("generate: no brief (pass as positional, --brief, --brief-file, or stdin)")
}

// noopCommitter satisfies codegen.Committer for --no-commit runs.
// Useful when an operator wants to inspect the generated files before
// they land in git.
type noopCommitter struct{}

func (noopCommitter) Commit(_ context.Context, _ string, _ string, _ string) error {
	return nil
}

// buildLens constructs a codegen.PromptLens from a state root +
// optional db URL. Returns (lens, cleanup, err); cleanup closes any
// db handles the graph builder opened. Either input can be empty:
//
//   - root="" + db="": returns nil, no-op cleanup, no error.
//     The generator falls back to brief-only behaviour.
//   - root!="" + db="":  knowledge corpus injection works; no graph slice.
//   - root!="" + db!="": both knowledge + graph inject (full lens).
//   - root=""  + db!="": graph works; no knowledge corpus injection.
func buildLens(root, dbURL string, log *slog.Logger) (*codegen.PromptLens, func(), error) {
	cleanup := func() {}

	var store *knowledge.Store
	if root != "" {
		s, err := knowledge.New(filepath.Join(root, "knowledge"))
		if err != nil {
			log.Warn("generate: knowledge unavailable", "err", err)
		} else {
			s.Git = &codegen.GitCommitter{}
			store = s
		}
	}

	var g *graph.Graph
	if dbURL != "" {
		j, jClose, err := openJournal(dbURL)
		if err != nil {
			log.Warn("generate: journal unavailable, graph slice disabled", "err", err)
		} else {
			credRepo, cClose, cerr := openCredentials(dbURL)
			if cerr != nil {
				log.Warn("generate: credentials unavailable, graph slice partial", "err", cerr)
				jClose()
			} else {
				cleanup = func() {
					cClose()
					jClose()
				}
				builder := &graph.Builder{Journal: j, Credentials: credRepo, Knowledge: store}
				built, berr := builder.Build(context.Background())
				if berr != nil {
					log.Warn("generate: graph build failed", "err", berr)
				} else {
					g = built
				}
			}
		}
	}

	return codegen.NewLens(store, g), cleanup, nil
}
