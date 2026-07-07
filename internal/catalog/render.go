package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// RenderForAI emits the catalog as Markdown for the codegen Environment Lens.
// For every service it states the base URL, auth scheme, suggested credential
// reference, and canonical operations, so the generator writes calls through
// sdk/http against verified metadata instead of guessing. Prefer an SDK package
// when one is listed.
func RenderForAI() string {
	var b strings.Builder
	b.WriteString("You can integrate any of these services. Use sdk/http inside a Step closure unless an SDK package is noted. ")
	b.WriteString("Fetch the credential at run time: API-key services use vault.MustGet(\"<key-name>\"); OAuth services use vault.MustGet(\"oauth:<connection-id>\"). ")
	b.WriteString("Confirm anything version- or instance-specific against the linked docs.\n")

	byCat := map[string][]Service{}
	for _, s := range All() {
		byCat[s.Category] = append(byCat[s.Category], s)
	}
	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	for _, cat := range cats {
		fmt.Fprintf(&b, "\n### %s\n", catTitle(cat))
		for _, s := range byCat[cat] {
			b.WriteString(renderService(s))
		}
	}
	return b.String()
}

func renderService(s Service) string {
	var b strings.Builder
	cred := "vault key \"" + s.KeyName + "\""
	if s.Auth == OAuth {
		cred = "oauth connection (vault id \"oauth:<connection-id>\")"
	}
	fmt.Fprintf(&b, "\n**%s** (`%s`) -- auth: %s; %s\n", s.Name, s.ID, cred, s.AuthScheme)
	fmt.Fprintf(&b, "- base URL: `%s`\n", s.BaseURL)
	if s.SDK != "" {
		fmt.Fprintf(&b, "- SDK available: `%s` (prefer it)\n", s.SDK)
	}
	for _, op := range s.Ops {
		path := op.Path
		if path == "" {
			path = "(base)"
		}
		fmt.Fprintf(&b, "- %s `%s` -- %s\n", op.Method, path, op.Summary)
	}
	fmt.Fprintf(&b, "- docs: %s\n", s.Docs)
	return b.String()
}

func catTitle(cat string) string {
	switch cat {
	case "crm":
		return "CRM"
	case "project-management":
		return "Project management"
	case "cms":
		return "CMS"
	case "email":
		return "Email"
	case "payments":
		return "Payments"
	case "dev":
		return "Developer tools"
	default:
		return cat
	}
}
