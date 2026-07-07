package codegen

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// AllowedImportPrefixes is the set of non-stdlib import path prefixes a
// generated or uploaded workflow may use. Everything in the standard
// library (import paths whose first segment contains no dot) is allowed
// implicitly; everything else must match one of these prefixes. This is
// the gate that stops a hostile brief or saved edit from pulling an
// arbitrary third-party module that could run attacker-controlled code
// (cgo directives, linker flags) at `go build` time.
var AllowedImportPrefixes = []string{
	"github.com/bright-interaction/reactor",
}

// deniedImports are import paths that PARSE as standard library (no dot in the
// first segment) but are forbidden regardless: they are the build-time-exec and
// sandbox-escape vectors the allowlist exists to stop. "C" is the cgo
// pseudo-import (a `#cgo`/`#include` preamble runs an arbitrary C compiler at
// build time); the rest give a workflow OS/process/memory reach it must never
// have. Checked in EVERY scanned file, not just main.go, so a subdir file
// cannot smuggle them past the lint pass.
var deniedImports = map[string]struct{}{
	"C":         {},
	"unsafe":    {},
	"plugin":    {},
	"os/exec":   {},
	"syscall":   {},
	"os/signal": {},
}

// CheckAllowedImports parses every .go file under dir (RECURSIVELY, including
// subdirectories) and returns an error if any import path is neither standard
// library nor under an allowed prefix, or is on the denylist. Workflows are
// sandboxed at runtime but `go build` is not, so this static check runs BEFORE
// any compilation. The recursion matters: an uploaded tarball can carry source
// in subdirectories that `go build .` would still compile.
func CheckAllowedImports(dir string) error {
	var disallowed []string
	seen := map[string]struct{}{}
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored/VCS trees; they never belong in a workflow module.
			if name := d.Name(); name == "vendor" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("import allowlist: parse %s: %w", path, perr)
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if importAllowed(p) {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			disallowed = append(disallowed, p)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("import allowlist: %w", walkErr)
	}
	if len(disallowed) > 0 {
		sort.Strings(disallowed)
		return fmt.Errorf("import allowlist: workflow imports disallowed package(s): %s (only the standard library and %s are permitted)",
			strings.Join(disallowed, ", "), strings.Join(AllowedImportPrefixes, ", "))
	}
	return nil
}

// importAllowed reports whether a single import path is permitted: stdlib
// (no dot in the first path segment) or under an allowed prefix, and NOT on
// the denylist.
func importAllowed(path string) bool {
	if _, denied := deniedImports[path]; denied {
		return false
	}
	first := path
	if i := strings.IndexByte(path, '/'); i >= 0 {
		first = path[:i]
	}
	if !strings.Contains(first, ".") {
		return true // standard library
	}
	for _, prefix := range AllowedImportPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// SecureBuildEnv returns the environment for an untrusted `go build`. It
// strips every REACTOR_* secret and the Anthropic key from the inherited
// environment (the build subprocess has no business reading the vault key
// or DB URL), then forces a hermetic, cgo-free toolchain:
//
//   - CGO_ENABLED=0 disables cgo, removing the #cgo directive vector that
//     lets a module invoke an arbitrary C compiler/linker at build time.
//   - GOTOOLCHAIN=local pins the toolchain so a hostile go.mod `toolchain`
//     line can't trigger a network toolchain download + exec.
//   - GOFLAGS=-mod=mod keeps module resolution working for the SDK import.
// buildEnvAllowlist is the set of environment variables the untrusted
// `go build` of workflow source may inherit. It is an ALLOWLIST, not a
// denylist. A denylist (strip REACTOR_/ARACHNE_/ANTHROPIC_) leaks every OTHER
// secret in the daemon's process env (CLOUDFLARE_TOKEN, AWS_SECRET_ACCESS_KEY,
// PGPASSWORD, GITHUB_TOKEN, ...) into a build that can execute attacker code at
// build time. Only toolchain-relevant, non-secret vars pass; the hermetic pins
// are appended explicitly below so an inherited GOFLAGS/GOPROXY/CGO_ENABLED
// cannot override them (last value wins in Go's env dedup).
var buildEnvAllowlist = map[string]struct{}{
	"PATH": {}, "HOME": {}, "TMPDIR": {}, "TMP": {}, "TEMP": {},
	"GOROOT": {}, "GOPATH": {}, "GOCACHE": {}, "GOMODCACHE": {}, "GOTMPDIR": {},
	"GOOS": {}, "GOARCH": {}, "GOARM": {}, "GOAMD64": {}, "GO386": {}, "GOMIPS": {},
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
}

func SecureBuildEnv() []string {
	env := make([]string, 0, 32)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if _, ok := buildEnvAllowlist[kv[:eq]]; ok {
			env = append(env, kv)
		}
	}
	// Hermetic, non-overridable pins (appended last so they beat any inherited
	// value): no cgo (kills the #cgo C-compiler exec vector), no network module
	// fetch, pinned toolchain, no on-disk go env file consulted, and -mod=mod
	// so the workflow-local SDK replace resolves from the warmed module cache.
	env = append(env,
		"CGO_ENABLED=0",
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOENV=off",
	)
	return env
}
