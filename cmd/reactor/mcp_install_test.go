package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout swaps os.Stdout for a pipe, runs fn, returns what was
// written. Lets us assert the JSON snippet shape without hitting the
// real terminal.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	orig := os.Stdout
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	err := fn()
	w.Close()
	<-done
	os.Stdout = orig
	return buf.String(), err
}

func TestMCPInstallEachClientPrintsValidJSON(t *testing.T) {
	for _, c := range availableClients {
		c := c
		t.Run(c, func(t *testing.T) {
			out, err := captureStdout(t, func() error {
				return cmdMCPInstall([]string{"--client", c, "--db", "sqlite://./test.db", "--root", "/tmp/test"})
			})
			if err != nil {
				t.Fatalf("install %s: %v", c, err)
			}
			// Strip the comment header lines so json.Unmarshal sees valid JSON.
			body := stripComments(out)
			var any interface{}
			if err := json.Unmarshal([]byte(body), &any); err != nil {
				t.Fatalf("install %s output is not JSON: %v\n--output--\n%s", c, err, out)
			}
		})
	}
}

func TestMCPInstallRejectsUnknownClient(t *testing.T) {
	t.Parallel()
	err := cmdMCPInstall([]string{"--client", "nope"})
	if err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Fatalf("got %v, want unknown-client error", err)
	}
}

func TestMCPInstallRequiresClient(t *testing.T) {
	t.Parallel()
	err := cmdMCPInstall([]string{})
	if err == nil || !strings.Contains(err.Error(), "--client required") {
		t.Fatalf("got %v, want --client required error", err)
	}
}

func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
