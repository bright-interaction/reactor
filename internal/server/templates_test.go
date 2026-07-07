package server

import (
	"strings"
	"testing"
)

func TestTemplatesBody(t *testing.T) {
	t.Parallel()
	on := templatesBody(true)
	if !strings.Contains(on, `action="/generate"`) || !strings.Contains(on, "Webhook to Slack alert") {
		t.Error("generator-on body should offer one-click build for a catalog template")
	}
	off := templatesBody(false)
	if strings.Contains(off, `action="/generate"`) {
		t.Error("generator-off body should not offer the one-click build form")
	}
	if !strings.Contains(off, "not configured") {
		t.Error("generator-off body should explain the builder is off")
	}
}
