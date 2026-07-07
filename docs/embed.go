// Package docs embeds the markdown documentation that ships alongside
// the reactor binary so the dashboard's /docs route can render it
// without requiring the operator to clone the repo. Add a new doc by
// dropping a .md file in this directory; the dashboard's docs viewer
// picks it up automatically.
package docs

import "embed"

// FS exposes every .md file in this directory. The server package
// reads from it via fs.ReadFile("README.md") etc; subdirs (e.g.
// .git/, ./_archive) are intentionally not embedded.
//
//go:embed *.md
var FS embed.FS
