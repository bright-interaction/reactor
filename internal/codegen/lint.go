package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Issue is one violation of a Reactor lint rule. The CLI prints these as
// `path:line:col: rule: message`; the codegen orchestrator joins them into
// a single error to drive its retry loop.
type Issue struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// Format renders an Issue as `path:line:col: rule: message`.
func (i Issue) Format() string {
	return fmt.Sprintf("%s:%d:%d: %s: %s", i.Path, i.Line, i.Col, i.Rule, i.Message)
}

// Rule names. The CLI uses these for --rules filtering.
const (
	RuleEmDash        = "em-dash"
	RuleBannedImport  = "banned-import"
	RuleBannedCall    = "banned-call"
	RuleBannedBuiltin = "banned-builtin"
)

var bannedImports = map[string]string{
	"math/rand":    "use the runtime's Step boundary; nondeterministic randomness breaks replay",
	"math/rand/v2": "same as math/rand",
	"os/exec":      "workflows must not shell out",
	"syscall":      "no raw syscalls",
	"unsafe":       "no unsafe pointer arithmetic in workflows",
}

// Lint reports every Reactor violation in src. path is used only for
// Issue.Path; src is the actual bytes to parse. Returns an empty slice on
// clean input. Parse errors surface as a single banned-call style Issue
// with rule="parse" so callers can render them uniformly.
//
// Forbidden:
//   - import of "math/rand", "math/rand/v2", "os/exec", "syscall", "unsafe"
//   - call to time.Sleep (use flow.Sleep)
//   - call to time.Now  (deterministic replay; record times inside Step closures)
//   - call to panic (use reactor.Permanent / Retryable wrappers)
//   - em dashes anywhere in the source
func Lint(src []byte, path string) []Issue {
	var issues []Issue

	// The character is U+2014 EM DASH. We use the unicode escape so this
	// file passes the very rule it enforces (and the repo-wide pre-commit
	// hook that scans newly added lines for raw em dashes).
	const emDash = "\u2014"
	if idx := indexAll(src, []byte(emDash)); len(idx) > 0 {
		for _, off := range idx {
			line, col := offsetLineCol(src, off)
			issues = append(issues, Issue{
				Path:    path,
				Line:    line,
				Col:     col,
				Rule:    RuleEmDash,
				Message: "em dashes are forbidden; use commas, colons, or parentheses",
			})
		}
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		issues = append(issues, Issue{
			Path: path, Line: 1, Col: 1,
			Rule:    "parse",
			Message: err.Error(),
		})
		return issues
	}

	for _, imp := range f.Imports {
		ipath := strings.Trim(imp.Path.Value, `"`)
		reason, banned := bannedImports[ipath]
		if !banned {
			continue
		}
		pos := fset.Position(imp.Pos())
		issues = append(issues, Issue{
			Path:    path,
			Line:    pos.Line,
			Col:     pos.Column,
			Rule:    RuleBannedImport,
			Message: fmt.Sprintf("import %q forbidden: %s", ipath, reason),
		})
	}

	// Track FuncLit nesting so time.Now is only flagged in workflow body
	// (outside closures). Step closures cache their output in the journal
	// so a time.Now there is captured once and replayed deterministically;
	// a time.Now in the workflow body re-runs on every replay and breaks
	// determinism. time.Sleep + panic stay banned everywhere because the
	// SDK provides durable replacements (flow.Sleep, reactor.Permanent).
	funcLitDepth := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			// ast.Inspect calls with nil to mark the post-order leave of
			// each node. We piggyback on that to drop FuncLit depth.
			return false
		}
		if _, isLit := n.(*ast.FuncLit); isLit {
			funcLitDepth++
			defer func() { funcLitDepth-- }()
			// Walk the FuncLit body explicitly so the depth bookkeeping
			// stays paired with the recursion.
			ast.Inspect(n.(*ast.FuncLit).Body, func(inner ast.Node) bool {
				return inspectCalls(inner, fset, path, &issues, funcLitDepth)
			})
			return false
		}
		return inspectCalls(n, fset, path, &issues, funcLitDepth)
	})

	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Line != issues[j].Line {
			return issues[i].Line < issues[j].Line
		}
		return issues[i].Col < issues[j].Col
	})
	return issues
}

// LintFile reads path from disk and lints it. Returns wrapped fs errors.
func LintFile(path string) ([]Issue, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lint: read %s: %w", path, err)
	}
	return Lint(src, path), nil
}

// LintDir walks root, lints every .go file (skipping _test.go and
// vendor/), and returns the concatenated issue list. Walks in lexical
// order so output is deterministic.
func LintDir(root string) ([]Issue, error) {
	var all []Issue
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		issues, err := LintFile(p)
		if err != nil {
			return err
		}
		all = append(all, issues...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// lintWorkflow is the legacy single-source helper the codegen orchestrator
// calls. It surfaces the issue list as one error string so the existing
// retry loop ("fix only the listed problems") sees every violation in one
// shot.
func lintWorkflow(src string) error {
	issues := Lint([]byte(src), "workflow.go")
	if len(issues) == 0 {
		return nil
	}
	parts := make([]string, 0, len(issues))
	for _, i := range issues {
		parts = append(parts, i.Format())
	}
	return fmt.Errorf("reactor lint: %s", strings.Join(parts, "; "))
}

// inspectCalls runs the per-CallExpr rules. Pulled out of the Lint
// closure so the FuncLit-depth bookkeeping has one re-entry point.
func inspectCalls(n ast.Node, fset *token.FileSet, path string, issues *[]Issue, funcLitDepth int) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return true
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if ident, ok := sel.X.(*ast.Ident); ok {
			switch {
			case ident.Name == "time" && sel.Sel.Name == "Sleep":
				// time.Sleep banned everywhere: use flow.Sleep so the
				// supervisor can suspend across long sleeps. Cheap to
				// catch; expensive to debug a wedged supervisor.
				pos := fset.Position(call.Pos())
				*issues = append(*issues, Issue{
					Path: path, Line: pos.Line, Col: pos.Column,
					Rule:    RuleBannedCall,
					Message: "time.Sleep forbidden; use flow.Sleep(ctx, name, duration)",
				})
			case funcLitDepth == 0 && ident.Name == "time" && sel.Sel.Name == "Now":
				addBodyOnly(call, fset, path, issues, "time.Now",
					"time.Now forbidden in workflow body; wrap with reactor.SideEffect or call inside a Step closure to capture once for replay")
			case funcLitDepth == 0 && ident.Name == "os" && sel.Sel.Name == "Getenv":
				addBodyOnly(call, fset, path, issues, "os.Getenv",
					"os.Getenv forbidden in workflow body; wrap with reactor.SideEffect to journal the value so replay sees the same env")
			case funcLitDepth == 0 && ident.Name == "rand" && (sel.Sel.Name == "Read" || sel.Sel.Name == "Int" || sel.Sel.Name == "Intn" || sel.Sel.Name == "Int63" || sel.Sel.Name == "Int63n" || sel.Sel.Name == "Float64"):
				addBodyOnly(call, fset, path, issues, "rand."+sel.Sel.Name,
					"rand."+sel.Sel.Name+" forbidden in workflow body; wrap with reactor.SideEffect or move into a Step closure")
			case funcLitDepth == 0 && ident.Name == "uuid" && (sel.Sel.Name == "New" || sel.Sel.Name == "NewString" || sel.Sel.Name == "NewRandom"):
				addBodyOnly(call, fset, path, issues, "uuid."+sel.Sel.Name,
					"uuid."+sel.Sel.Name+" forbidden in workflow body; wrap with reactor.SideEffect so replay returns the same id")
			}
		}
	}
	if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "panic" {
		pos := fset.Position(call.Pos())
		*issues = append(*issues, Issue{
			Path: path, Line: pos.Line, Col: pos.Column,
			Rule:    RuleBannedBuiltin,
			Message: "panic forbidden; wrap errors with reactor.Permanent or Retryable",
		})
	}
	return true
}

// addBodyOnly is the shared "outside FuncLit" issue emitter. The
// _ pkgName param is unused at the moment but kept so future log lines
// can prefix the rule with the package without re-shaping callers.
func addBodyOnly(call *ast.CallExpr, fset *token.FileSet, path string, issues *[]Issue, _, msg string) {
	pos := fset.Position(call.Pos())
	*issues = append(*issues, Issue{
		Path:    path,
		Line:    pos.Line,
		Col:     pos.Column,
		Rule:    RuleBannedCall,
		Message: msg,
	})
}

// indexAll returns every byte offset where needle starts in haystack.
func indexAll(haystack, needle []byte) []int {
	var out []int
	start := 0
	for {
		i := strings.Index(string(haystack[start:]), string(needle))
		if i < 0 {
			return out
		}
		out = append(out, start+i)
		start += i + len(needle)
		if start >= len(haystack) {
			return out
		}
	}
}

// offsetLineCol returns the 1-indexed line + column for a byte offset.
func offsetLineCol(src []byte, off int) (int, int) {
	if off > len(src) {
		off = len(src)
	}
	line := 1
	col := 1
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col
}
