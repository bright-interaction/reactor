package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/reactor/internal/codegen"
)

// Lint fixtures live under testdata/lint and are generated at test time
// rather than committed to disk. The em-dash fixture intentionally
// contains a U+2014 character; committing it would trip the repo-wide
// pre-commit hook that bans em dashes in tracked source.
const (
	cleanSrc = `package workflow

import (
	"context"
	"errors"

	"github.com/bright-interaction/reactor/sdk"
)

var Workflow = reactor.Workflow{Slug: "demo", Version: "0.1.0"}
var Trigger = reactor.WebhookTrigger{Path: "/demo", Provider: "generic"}

type empty struct{}

func Run(ctx context.Context, flow reactor.Flow, _ empty) error {
	if flow == nil {
		return errors.New("nil flow")
	}
	return nil
}
`
	badImportSrc = `package workflow

import (
	"context"
	"math/rand"

	"github.com/bright-interaction/reactor/sdk"
)

var Workflow = reactor.Workflow{Slug: "rand-demo", Version: "0.1.0"}

type empty struct{}

func RunRand(ctx context.Context, flow reactor.Flow, _ empty) error {
	_ = rand.Int()
	return nil
}
`
	bannedCallSrc = `package workflow

import (
	"context"
	"time"
)

func Snore(ctx context.Context) {
	time.Sleep(time.Second)
}
`
)

// emDashSrc is built from the U+2014 unicode escape so this test file
// itself stays em-dash-free and survives the pre-commit hook.
var emDashSrc = "package workflow\n\n// trigger rule via \u2014 here\n\nimport \"context\"\n\nfunc Stub(_ context.Context) {}\n"

// writeFixtures materialises the four canonical fixtures into a fresh
// temp dir and returns the directory + per-file paths.
func writeFixtures(t *testing.T) (dir, clean, badImport, banned, emDash string) {
	t.Helper()
	dir = t.TempDir()
	files := map[string]string{
		"clean.go":       cleanSrc,
		"bad_import.go":  badImportSrc,
		"banned_call.go": bannedCallSrc,
		"em_dash.go":     emDashSrc,
	}
	for name, src := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return dir,
		filepath.Join(dir, "clean.go"),
		filepath.Join(dir, "bad_import.go"),
		filepath.Join(dir, "banned_call.go"),
		filepath.Join(dir, "em_dash.go")
}

func TestLintCleanFixturePasses(t *testing.T) {
	t.Parallel()
	_, clean, _, _, _ := writeFixtures(t)
	issues, err := codegen.LintFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected clean, got %v", issues)
	}
}

func TestLintBadImportFixture(t *testing.T) {
	t.Parallel()
	_, _, badImport, _, _ := writeFixtures(t)
	issues, err := codegen.LintFile(badImport)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) == 0 {
		t.Fatal("expected at least one issue")
	}
	found := false
	for _, i := range issues {
		if i.Rule == codegen.RuleBannedImport && strings.Contains(i.Message, "math/rand") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing banned-import for math/rand: %v", issues)
	}
}

func TestLintEmDashFixture(t *testing.T) {
	t.Parallel()
	_, _, _, _, emDash := writeFixtures(t)
	issues, err := codegen.LintFile(emDash)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) == 0 {
		t.Fatal("expected em-dash issue")
	}
	found := false
	for _, i := range issues {
		if i.Rule == codegen.RuleEmDash {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing em-dash issue: %v", issues)
	}
}

func TestLintBannedCallFixture(t *testing.T) {
	t.Parallel()
	_, _, _, banned, _ := writeFixtures(t)
	issues, err := codegen.LintFile(banned)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range issues {
		if i.Rule == codegen.RuleBannedCall && strings.Contains(i.Message, "time.Sleep") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing time.Sleep banned-call: %v", issues)
	}
}

func TestCmdLintExitCodes(t *testing.T) {
	t.Parallel()
	_, clean, badImport, _, _ := writeFixtures(t)
	if err := cmdLint([]string{clean}); err != nil {
		t.Fatalf("clean fixture failed: %v", err)
	}
	if err := cmdLint([]string{badImport}); err == nil {
		t.Fatal("bad-import fixture passed lint, expected failure")
	}
}

func TestCmdLintRulesFilter(t *testing.T) {
	t.Parallel()
	_, _, badImport, _, _ := writeFixtures(t)
	// Filter to only em-dash; bad_import.go has no em dash so we expect zero issues.
	if err := cmdLint([]string{"--rules", codegen.RuleEmDash, badImport}); err != nil {
		t.Fatalf("rules filter should hide banned-import: %v", err)
	}
}
