package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/brightinteraction/reactor/internal/codegen"
)

// generateWorkflow handles POST /generate. Body has a "brief" form
// field; the handler runs the codegen orchestrator, builds the
// resulting binary, registers it in the journal so the dispatcher can
// find it, and redirects to /workflows/{slug}. Synchronous; takes
// 10-60s depending on the model + retry count.
func (s *Server) generateWorkflow(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	brief := strings.TrimSpace(r.PostFormValue("brief"))
	if brief == "" {
		http.Error(w, "brief is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	slug, path, version, err := s.Generator.GenerateFromBrief(ctx, brief)
	if err != nil {
		s.errorPage(w, "generate", err)
		return
	}

	if err := s.autoBuildAndRegister(ctx, slug, path, version); err != nil {
		// Workflow code landed on disk + was committed, but the build
		// or register step failed. Surface that on the error page so
		// the operator knows the codegen output is preserved and only
		// the post-step needs follow-up.
		s.errorPage(w, "generate post-step ("+slug+" at "+path+")", err)
		return
	}

	http.Redirect(w, r, "/workflows/"+slug, http.StatusSeeOther)
}

// autoBuildAndRegister mirrors `reactor workflow build` + `register`
// without going through the CLI. Run after a successful Generate so
// the operator's redirect lands on a usable detail page. Thin wrapper
// over codegen.BuildAndRegister which carries the canonical pipeline.
func (s *Server) autoBuildAndRegister(ctx context.Context, slug, src, version string) error {
	root := s.State
	if root == "" && s.Registry != nil {
		root = s.Registry.Root
	}
	if root == "" {
		return fmt.Errorf("no state dir wired (set Server.State); generated workflow at %s needs a manual `reactor workflow build`", src)
	}
	_, err := codegen.BuildAndRegister(ctx, s.Journal, codegen.BuildAndRegisterRequest{
		Slug:         slug,
		SrcDir:       src,
		StateRoot:    root,
		SDKVersion:   version,
		SkipIfExists: true,
	})
	return err
}
