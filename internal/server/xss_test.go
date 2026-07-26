package server

import (
	"strings"
	"testing"

	"github.com/bright-interaction/reactor/internal/auth"
)

// These pin the escaping of attacker-controlled names in destructive-action
// confirmations. The context CHANGED: the message used to sit in an inline
// onsubmit="return confirm('...')" JS string literal, which the dashboard's own
// CSP (script-src 'self', no 'unsafe-inline') silently blocked, so every confirm
// was dead. It now sits in a data-confirm HTML ATTRIBUTE read by a delegated
// listener in /assets/ui.js, which flips the correct escaper from
// JSEscapeString to HTMLEscapeString.
//
// The previous versions of these tests asserted the JS-literal context and kept
// passing after the move for the wrong reason: they looked for a bare quote
// anywhere in the page, which unrelated static copy satisfies. The assertions
// below are about the payload itself and the attribute boundary.

// attackerBreakout tries to terminate a double-quoted HTML attribute and inject
// an event handler, which is the actual escape a data-confirm value must resist.
const attackerBreakout = `evil" onmouseover="alert(document.cookie)`

func TestUsersBodyConfirmAttributeEscaping(t *testing.T) {
	t.Parallel()
	list := []auth.User{{ID: "u1", Username: attackerBreakout, Role: auth.RoleMember}}
	// me is a different admin so the row renders its action buttons rather
	// than the "(you)" placeholder.
	me := auth.User{ID: "admin", Username: "admin", Role: auth.RoleAdmin}
	html := usersBody(list, me, "")

	assertNoAttributeBreakout(t, html)
	if !strings.Contains(html, `data-confirm="Delete user `) {
		t.Fatal("delete-user form lost its data-confirm; without it the destructive action has no confirmation at all")
	}
}

func TestTokensBodyConfirmAttributeEscaping(t *testing.T) {
	t.Parallel()
	html := tokensBody([]auth.APIToken{{ID: "t1", Name: attackerBreakout}}, "")

	assertNoAttributeBreakout(t, html)
	if !strings.Contains(html, `data-confirm="Revoke token `) {
		t.Fatal("revoke-token form lost its data-confirm")
	}
}

// TestNoInlineEventHandlersAnywhere is the structural guard. Any inline handler
// attribute is dead on arrival under this CSP, so reintroducing one silently
// removes a confirmation or breaks a form. Rendering a representative page set
// and scanning is cheaper than trusting review.
func TestNoInlineEventHandlersAnywhere(t *testing.T) {
	t.Parallel()
	me := auth.User{ID: "admin", Username: "admin", Role: auth.RoleAdmin}
	pages := map[string]string{
		"users":  usersBody([]auth.User{{ID: "u1", Username: "bob", Role: auth.RoleMember}}, me, ""),
		"tokens": tokensBody([]auth.APIToken{{ID: "t1", Name: "ci"}}, ""),
	}
	for name, html := range pages {
		for _, handler := range []string{"onsubmit=", "onchange=", "onclick=", "onload=", "onerror="} {
			if strings.Contains(html, handler) {
				t.Fatalf("%s page contains inline %s, which the CSP blocks; use data-confirm or data-reveal-prefix and /assets/ui.js", name, handler)
			}
		}
	}
}

func assertNoAttributeBreakout(t *testing.T, html string) {
	t.Helper()
	// The raw payload must never appear: an unescaped double quote would close
	// data-confirm and the rest would land as live attributes.
	if strings.Contains(html, attackerBreakout) {
		t.Fatalf("payload reached the page unescaped, breaking out of the attribute:\n%s", html)
	}
	// Tighter: the payload's own double quote must be escaped. Everything after
	// it is inert TEXT inside the attribute value as long as this holds, so
	// checking for `onmouseover=` directly would be wrong (the escaped form
	// legitimately contains that substring).
	if strings.Contains(html, `evil"`) {
		t.Fatalf("payload's double quote survived unescaped, which closes data-confirm:\n%s", html)
	}
	// And it must appear in its escaped form, proving it was rendered (not
	// dropped) and that the escaper ran.
	if !strings.Contains(html, `evil&#34; onmouseover=&#34;`) {
		t.Fatalf("expected the payload HTML-escaped (&#34;) inside data-confirm:\n%s", html)
	}
}
