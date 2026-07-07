package server

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

// parseForm runs r.ParseForm and reports any required fields the body
// is missing. Returns (form, ""), (form, "first missing field name"),
// or sends a 400 + returns (nil, "form parse error") on a transport
// level parse failure. Handlers commonly want different render-on
// missing strategies; this returns the diagnostic instead of writing
// the response so the caller decides between http.Error and re-rendering
// the form with a 422 + inline error pill.
func parseForm(w http.ResponseWriter, r *http.Request, required ...string) (url.Values, string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return nil, "_parse_error"
	}
	for _, field := range required {
		if strings.TrimSpace(r.PostFormValue(field)) == "" {
			return r.PostForm, field
		}
	}
	return r.PostForm, ""
}

// renderFormError emits a 422 with the given inline error message
// rendered via the supplied body function. The body function takes the
// error string and returns the HTML for the form region; callers feed
// in their existing knowledgeNewBody / workflowNewBody / etc.
func renderFormError(w http.ResponseWriter, title string, body func(errMsg string) string, msg string) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	render(w, layout, page{
		Title:   title,
		Heading: title,
		Body:    template.HTML(body(msg)),
	})
}

// trimmed returns strings.TrimSpace of the form field; tiny convenience
// for handlers that read many fields and want the call sites compact.
func trimmed(values url.Values, key string) string {
	return strings.TrimSpace(values.Get(key))
}
