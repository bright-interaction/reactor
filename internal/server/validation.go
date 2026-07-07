package server

import (
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
)

// slugRe is the same shape codegen enforces on workflow slugs. Lives
// here too so the HTTP layer can reject bad slugs at handler entry
// without importing internal/codegen (which would pull the Anthropic
// client onto every read-only deployment).
var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// slugFromRequest reads the {slug} path parameter, validates it against
// the canonical slug shape, and writes a 400 + returns ok=false when
// the value is missing or unsafe. Used by every handler that joins
// the slug into a filesystem path or feeds it to journal lookups.
//
// chi does not match `/` inside path params but does accept `..`, `.`,
// percent-encoded variants, leading dots, and unicode lookalikes; any
// of those would break filesystem assumptions downstream.
func slugFromRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	slug := chi.URLParam(r, "slug")
	if !slugRe.MatchString(slug) {
		http.Error(w, "invalid slug", http.StatusBadRequest)
		return "", false
	}
	return slug, true
}
