package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bright-interaction/reactor/internal/codegen"
)

// cmdLint walks the path argument, lints every .go file (or just the
// single file if path is one), and prints the issues. Exit code 1 on any
// violation, 0 on clean. Designed as a pre-commit gate for hand-written
// workflow Go files; the codegen orchestrator already calls the same
// linter in-process before committing AI-generated workflows.
func cmdLint(args []string) error {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	format := fs.String("format", "text", "output format: text or json")
	rules := fs.String("rules", "", "comma-separated rule names to apply (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("missing path argument")
	}

	enabled := parseRuleFilter(*rules)

	var allIssues []codegen.Issue
	for _, path := range fs.Args() {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		var issues []codegen.Issue
		if info.IsDir() {
			issues, err = codegen.LintDir(path)
		} else {
			issues, err = codegen.LintFile(path)
		}
		if err != nil {
			return err
		}
		for _, i := range issues {
			if enabled != nil && !enabled[i.Rule] {
				continue
			}
			allIssues = append(allIssues, i)
		}
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(allIssues); err != nil {
			return err
		}
	case "text", "":
		for _, i := range allIssues {
			fmt.Fprintln(os.Stdout, i.Format())
		}
	default:
		return fmt.Errorf("unknown --format %q (want text or json)", *format)
	}

	if len(allIssues) > 0 {
		return fmt.Errorf("lint: %d issue(s)", len(allIssues))
	}
	return nil
}

// parseRuleFilter returns nil to mean "all rules"; otherwise a set of
// enabled rule names.
func parseRuleFilter(raw string) map[string]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, r := range strings.Split(raw, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		out[r] = true
	}
	return out
}
