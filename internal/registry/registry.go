// Package registry resolves a workflow slug to the compiled binary the
// supervisor exec's. Production deployments store binaries under
// <root>/<slug>/workflow; tests inject a fake.
//
// Convention is simple by design: one directory per workflow, the
// binary always named "workflow". This matches the codegen
// orchestrator's atomic rename target (reactor-workflows/<slug>/) so
// `reactor workflow build <slug>` writes into the same shape the
// daemon reads from.
package registry

import (
	"fmt"
	"os"
	"path/filepath"
)

// FileRegistry resolves slugs against a root directory.
type FileRegistry struct {
	Root string
}

// New returns a FileRegistry rooted at the given directory.
func New(root string) *FileRegistry {
	return &FileRegistry{Root: root}
}

// BinaryPath implements dispatcher.BinaryLookup. Returns an error if
// the binary doesn't exist or isn't executable.
func (r *FileRegistry) BinaryPath(slug string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("registry: empty slug")
	}
	path := filepath.Join(r.Root, slug, "workflow")
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("registry: stat %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("registry: %s is a directory", path)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("registry: %s is not executable", path)
	}
	return path, nil
}

// List returns every slug with a deployable binary under Root. Used by
// the status server's home page so operators can see what's available.
func (r *FileRegistry) List() ([]string, error) {
	entries, err := os.ReadDir(r.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("registry: read dir: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := r.BinaryPath(e.Name()); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
