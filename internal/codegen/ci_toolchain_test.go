package codegen

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	dockerGoRe = regexp.MustCompile(`FROM[^\n]*golang:([0-9]+\.[0-9]+\.[0-9]+)-alpine`)
	ciGoRe     = regexp.MustCompile(`go-version:\s*"([^"]+)"`)
)

// TestCIGoVersionMatchesDockerfile turns a two-file invariant into a test.
//
// The Dockerfile pins its golang tag deliberately: its own comment records that
// 1.26.5 was chosen to pick up the crypto/tls fix for GO-2026-5856, and warns
// that the failure mode is asymmetric --
//
//	a CI image OLDER than this stage fails loudly red, a NEWER one fails
//	silently green
//
// The repo's own GitHub Actions were pinned to 1.22 while go.mod required
// 1.25.10, so GOTOOLCHAIN=auto silently fetched 1.25.10 and every release
// binary, .deb, .rpm and Homebrew bottle shipped a stdlib OLDER than the
// container's. Green the whole time.
//
// A comment asking a human to keep two files in step is not an invariant, it is
// a countdown. This is the same lesson as the playwright image pin that drifted
// twice with a warning comment sitting right above it.
func TestCIGoVersionMatchesDockerfile(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..") // reactor/

	raw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	matches := dockerGoRe.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("no golang:X.Y.Z-alpine FROM found in the Dockerfile; update this guard if the base image changed")
	}
	want := matches[0][1]
	for _, m := range matches[1:] {
		if m[1] != want {
			t.Fatalf("the Dockerfile's own stages disagree: %s vs %s (the runtime stage compiles user workflows; both must carry the patched stdlib)", want, m[1])
		}
	}

	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflow files found; update this guard if CI moved")
	}
	checked := 0
	for _, wf := range workflows {
		body, rerr := os.ReadFile(wf)
		if rerr != nil {
			t.Fatalf("read %s: %v", wf, rerr)
		}
		for _, m := range ciGoRe.FindAllStringSubmatch(string(body), -1) {
			checked++
			if m[1] != want {
				t.Fatalf("%s pins go-version %q but the Dockerfile pins %q; release artifacts would ship a different stdlib than the image, and an OLDER CI toolchain fails silently green",
					filepath.Base(wf), m[1], want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no go-version pin found in any workflow; this guard would pass vacuously")
	}
}

// TestCIRunsGovulncheck keeps the vulnerability gate from quietly disappearing.
// The Dockerfile's Go pin exists BECAUSE of a CVE, so a pipeline that never
// scans is a pin nobody is checking.
func TestCIRunsGovulncheck(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "test.yml"))
	if err != nil {
		t.Fatalf("read test.yml: %v", err)
	}
	if !strings.Contains(string(body), "govulncheck") {
		t.Fatal("test.yml no longer runs govulncheck; the public CI would stop checking the toolchain and deps for known vulnerabilities")
	}
}
