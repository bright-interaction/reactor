package server

import (
	"strings"
	"testing"

	"github.com/bright-interaction/reactor/internal/auth"
)

// TestUsersBodyOnsubmitXSS pins the fix for the stored-XSS hole where a
// username was HTML-escaped (wrong) inside an onsubmit JS string literal,
// letting a quote break out and run script. JSEscapeString must neutralise
// the payload so the raw breakout sequence never reaches the attribute.
func TestUsersBodyOnsubmitXSS(t *testing.T) {
	payload := `evil');alert(document.cookie);//`
	list := []auth.User{
		{ID: "u1", Username: payload, Role: auth.RoleMember},
	}
	// me is a different admin so the row renders its action buttons
	// (the onsubmit form), not the "(you)" placeholder.
	me := auth.User{ID: "admin", Username: "admin", Role: auth.RoleAdmin}
	html := usersBody(list, me, "")

	// The literal breakout sequence must NOT appear: a real ' would let
	// the JS string close early. JSEscapeString renders it as '.
	if strings.Contains(html, `evil');alert`) {
		t.Fatalf("username broke out of the onsubmit JS string (unescaped quote):\n%s", html)
	}
	if !strings.Contains(html, `'`) {
		t.Fatalf("expected the quote to be JS-escaped (\\u0027) in the output")
	}
}

func TestTokensBodyOnsubmitXSS(t *testing.T) {
	payload := `tok');alert(1);//`
	list := []auth.APIToken{
		{ID: "t1", Name: payload},
	}
	html := tokensBody(list, "")
	if strings.Contains(html, `tok');alert`) {
		t.Fatalf("token name broke out of the onsubmit JS string:\n%s", html)
	}
}
