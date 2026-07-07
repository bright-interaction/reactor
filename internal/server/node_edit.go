package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bright-interaction/reactor/internal/codeedit"
	"github.com/go-chi/chi/v5"
)

// node_edit.go is the code-aware bridge behind the visual workflow editor. The
// DAG nodes the operator clicks map one-to-one to named steps in the workflow
// source; these two handlers let the drawer fetch a single node's code and save
// an edit to just that node, splicing it back into the full file and running it
// through the same validator as a whole-file save. The platform stays aware of
// the code, so a non-dev edits one node in a focused panel instead of the whole
// program, and a dev still gets the raw Go.

// nodeStep reads and lightly bounds the {step} path parameter.
func nodeStep(w http.ResponseWriter, r *http.Request) (string, bool) {
	step := chi.URLParam(r, "step")
	if step == "" || len(step) > 200 {
		http.Error(w, "invalid step name", http.StatusBadRequest)
		return "", false
	}
	return step, true
}

// workflowNodeCode serves GET /workflows/{slug}/node/{step}/code: the Go source
// of one step, as JSON, for the slide-in editor drawer.
func (s *Server) workflowNodeCode(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromRequest(w, r)
	if !ok {
		return
	}
	step, ok := nodeStep(w, r)
	if !ok {
		return
	}

	dir := filepath.Join(s.WorkflowsRoot, slug)
	codeBytes, codePath := readFirstAvailable(dir, "main.go", "workflow.go")
	if len(codeBytes) == 0 {
		http.Error(w, "workflow source not bundled", http.StatusNotFound)
		return
	}

	snippet, err := codeedit.ExtractStep(string(codeBytes), step)
	if err != nil {
		var nf *codeedit.ErrNotFound
		if errors.As(err, &nf) {
			http.Error(w, "no code found for this node", http.StatusNotFound)
			return
		}
		http.Error(w, "could not read node code: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	upstream, downstream, runID := s.nodeDataflow(r.Context(), slug, dir, step)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"slug":       slug,
		"step":       step,
		"code":       snippet,
		"editable":   s.CodeValidator != nil,
		"source":     filepath.Base(codePath),
		"upstream":   upstream,
		"downstream": downstream,
		"sample_run": runID,
	})
}

// workflowSaveNodeCode handles POST /workflows/{slug}/node/{step}/code: it
// splices the edited snippet back into the full source and saves it through the
// same validate+commit path as a whole-file edit. A snippet that fails to
// compile is rejected with the validator's message, so a bad node edit cannot
// corrupt the workflow.
func (s *Server) workflowSaveNodeCode(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromRequest(w, r)
	if !ok {
		return
	}
	step, ok := nodeStep(w, r)
	if !ok {
		return
	}
	if s.CodeValidator == nil {
		http.Error(w, "code validator not wired", http.StatusServiceUnavailable)
		return
	}

	snippet, status, err := readEditBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	dir := filepath.Join(s.WorkflowsRoot, slug)
	codeBytes, _ := readFirstAvailable(dir, "main.go", "workflow.go")
	if len(codeBytes) == 0 {
		http.Error(w, "workflow source not bundled", http.StatusNotFound)
		return
	}

	merged, err := codeedit.ReplaceStep(string(codeBytes), step, string(snippet))
	if err != nil {
		var nf *codeedit.ErrNotFound
		if errors.As(err, &nf) {
			http.Error(w, "no code found for this node", http.StatusNotFound)
			return
		}
		http.Error(w, "could not splice node code: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	dagBytes, _ := os.ReadFile(filepath.Join(dir, "dag.json"))
	if status, err := s.writeValidatedCode(r.Context(), slug, dir, []byte(merged), dagBytes); err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "step": step})
}
