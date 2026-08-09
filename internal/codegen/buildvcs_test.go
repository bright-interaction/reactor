package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryGoBuildDisablesVCSStamping is a guard, not a unit test.
//
// `go build` stamps VCS info by default, so it shells out to git whenever the
// source being compiled sits inside a repository. Reactor's README tells
// operators to `git init` their state directory, the Dockerfile ships git, and
// the daemon normally runs as a different uid than whoever created that repo
// (the systemd `reactor` user, uid 65532 in the image). git then reports
// "detected dubious ownership", exits 128, and the compile dies with:
//
//	error obtaining VCS status: exit status 128
//	    Use -buildvcs=false to disable VCS stamping.
//
// Every workflow build fails and the operator sees only "go build: exit status
// 1" with the real cause buried in a wrapped error. Nothing in Reactor reads the
// stamp, so the flag costs nothing.
//
// This scans the tree rather than asserting on one call site because the defect
// is an incomplete-migration shape: there were three separate `go build`
// invocations and a fourth in a test, and fixing only the one you happened to
// find leaves the next one to rediscover it.
func TestEveryGoBuildDisablesVCSStamping(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..") // reactor/

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "reference-repos":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if d.Name() == "buildvcs_test.go" {
			return nil // this file's own detector strings would self-match
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// The two shapes Reactor uses to spawn a compile.
			isExec := strings.Contains(line, `exec.Command`) && strings.Contains(line, `"build"`)
			isRun := strings.Contains(line, `run(ctx,`) && strings.Contains(line, `"build"`)
			if !isExec && !isRun {
				continue
			}
			// Either the named constant (preferred) or the raw literal, since
			// packages that cannot import codegen still have to pass it.
			if strings.Contains(line, "BuildVCSFlag") || strings.Contains(line, "-buildvcs=false") {
				continue
			}
			offenders = append(offenders,
				filepath.ToSlash(path)+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("go build invocation(s) without %s -- a workflow tree inside a git repo the daemon uid cannot read will fail to compile:\n  %s",
			BuildVCSFlag, strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
