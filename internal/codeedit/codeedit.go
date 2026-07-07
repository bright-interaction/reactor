// Package codeedit is the code-aware layer behind the visual workflow editor:
// given a workflow's Go source and a step name, it locates that step's
// statement and can extract or replace it. The platform stays "aware of the
// code" so a node clicked in the visual DAG maps to exactly its slice of source
// -- editable in a focused drawer instead of the whole file. Edits are spliced
// back as text and then handed to the normal validator (vet + build), so a
// broken edit is rejected: this package only has to LOCATE safely, not
// understand arbitrary Go.
package codeedit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
)

// stepFuncs are the SDK calls that introduce a named step.
var stepFuncs = map[string]bool{"Step": true, "SideEffect": true}

// ErrNotFound is returned when no step with the given name exists in the source.
type ErrNotFound struct{ Name string }

func (e *ErrNotFound) Error() string { return fmt.Sprintf("codeedit: step %q not found", e.Name) }

// locate returns the byte offsets [start,end) of the smallest statement that
// contains the reactor.Step/SideEffect call whose name literal equals name.
func locate(src, name string) (start, end int, err error) {
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, "workflow.go", src, parser.ParseComments)
	if perr != nil {
		return 0, 0, fmt.Errorf("codeedit: parse: %w", perr)
	}
	var call *ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !stepFuncs[funcName(ce.Fun)] {
			return true
		}
		if firstStringLit(ce.Args) == name {
			call = ce
			return false
		}
		return true
	})
	if call == nil {
		return 0, 0, &ErrNotFound{Name: name}
	}
	// Smallest block-level statement enclosing the call. We only consider
	// statements that are direct members of a block's list (the whole `if`, the
	// whole assignment) rather than sub-clauses like an `if`'s init statement --
	// the meaningful editable unit is the statement plus its error handling. The
	// deepest such block wins, so a Step nested inside an `if` body resolves to
	// that inner statement.
	var best ast.Stmt
	ast.Inspect(f, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for _, st := range block.List {
			if st.Pos() <= call.Pos() && call.End() <= st.End() {
				if best == nil || st.Pos() >= best.Pos() {
					best = st
				}
			}
		}
		return true
	})
	if best == nil {
		return 0, 0, fmt.Errorf("codeedit: no enclosing statement for %q", name)
	}
	return fset.Position(best.Pos()).Offset, fset.Position(best.End()).Offset, nil
}

// ExtractStep returns the source text of the statement that defines step name.
func ExtractStep(src, name string) (string, error) {
	start, end, err := locate(src, name)
	if err != nil {
		return "", err
	}
	return src[start:end], nil
}

// ReplaceStep splices replacement in place of step name's statement and returns
// the new source. The caller MUST validate the result compiles (the editor runs
// it through the vet+build validator); this only does the textual splice.
func ReplaceStep(src, name, replacement string) (string, error) {
	start, end, err := locate(src, name)
	if err != nil {
		return "", err
	}
	return src[:start] + replacement + src[end:], nil
}

// StepNames lists every named step in source order (for sanity-checking the DAG
// against the code).
func StepNames(src string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "workflow.go", src, 0)
	if err != nil {
		return nil, fmt.Errorf("codeedit: parse: %w", err)
	}
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if stepFuncs[funcName(ce.Fun)] {
			if s := firstStringLit(ce.Args); s != "" {
				names = append(names, s)
			}
		}
		return true
	})
	return names, nil
}

// funcName returns the called function's identifier (e.g. "Step" for
// reactor.Step or a bare Step).
func funcName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr: // generic call: Step[T](...)
		return funcName(x.X)
	case *ast.IndexListExpr:
		return funcName(x.X)
	}
	return ""
}

// firstStringLit returns the unquoted value of the first string-literal arg, or
// "" if none. The step name is the first string literal in the call.
func firstStringLit(args []ast.Expr) string {
	for _, a := range args {
		if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if len(lit.Value) >= 2 {
				return lit.Value[1 : len(lit.Value)-1]
			}
		}
	}
	return ""
}
