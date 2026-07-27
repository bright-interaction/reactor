package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bright-interaction/reactor/internal/auth"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// TestMemberIsNotLockedOutOfTheirOwnSlug covers a correctness bug created by
// making slugs unique PER TENANT while the dashboard still addresses workflows by
// slug in the URL.
//
// The unscoped WorkflowIDBySlug resolves "WHERE slug = $1 ORDER BY created_at
// DESC LIMIT 1". Once a second tenant registered the same slug, every handler
// resolved to whichever tenant registered it MOST RECENTLY. The downstream scope
// check then correctly refused, so this was never a leak, but the member could no
// longer open a workflow they own: the other tenant's newer row shadowed theirs
// permanently, and the page said "workflow not registered" about their own
// workflow.
//
// Resolving within the viewer's tenant fixes it. Admins have an empty scope and
// WorkflowIDBySlugInTenant falls back to the unscoped lookup, so their behaviour
// is unchanged.
func TestMemberIsNotLockedOutOfTheirOwnSlug(t *testing.T) {
	t.Parallel()
	_, j, _ := newTestServer(t)
	ctx := context.Background()

	// The member's own workflow, registered FIRST.
	if err := j.CreateWorkflowInTenant(ctx, "wf_mine", "billing", "h", "0.1.0", json.RawMessage(`{}`), "acme"); err != nil {
		t.Fatal(err)
	}
	// Another tenant claims the same slug LATER, so it wins the unscoped
	// ORDER BY created_at DESC.
	if err := j.CreateWorkflowInTenant(ctx, "wf_theirs", "billing", "h", "0.1.0", json.RawMessage(`{}`), "globex"); err != nil {
		t.Fatal(err)
	}

	s := &Server{Journal: j, BasicAuth: BasicAuthConfig{AllowNoAuth: true}}
	member := auth.User{ID: "m", Role: auth.RoleMember, TenantID: "acme"}
	req := httptest.NewRequest(http.MethodGet, "/workflows/billing", nil)
	req = req.WithContext(withUser(req.Context(), member))

	got, err := s.workflowIDForViewer(req, "billing")
	if err != nil {
		t.Fatalf("the member cannot resolve their own slug at all: %v", err)
	}
	if got != "wf_mine" {
		t.Fatalf("member resolved %q, want wf_mine. The other tenant's newer row shadowed theirs, so the page 404s on a workflow they own.", got)
	}

	// The other tenant's member gets THEIR row, not the newest one globally.
	other := auth.User{ID: "o", Role: auth.RoleMember, TenantID: "globex"}
	req2 := httptest.NewRequest(http.MethodGet, "/workflows/billing", nil)
	req2 = req2.WithContext(withUser(req2.Context(), other))
	got2, err := s.workflowIDForViewer(req2, "billing")
	if err != nil || got2 != "wf_theirs" {
		t.Fatalf("globex member resolved (%q, %v), want wf_theirs", got2, err)
	}

	// A tenant owning no such slug gets ErrNotFound rather than someone else's.
	third := auth.User{ID: "t", Role: auth.RoleMember, TenantID: "initech"}
	req3 := httptest.NewRequest(http.MethodGet, "/workflows/billing", nil)
	req3 = req3.WithContext(withUser(req3.Context(), third))
	if id, err := s.workflowIDForViewer(req3, "billing"); err == nil {
		t.Fatalf("unrelated tenant resolved %q, want an error", id)
	} else if !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// An ADMIN keeps the unscoped behaviour: empty scope falls back, so admin
	// surfaces that legitimately span tenants do not change.
	admin := auth.User{ID: "a", Role: auth.RoleAdmin}
	req4 := httptest.NewRequest(http.MethodGet, "/workflows/billing", nil)
	req4 = req4.WithContext(withUser(req4.Context(), admin))
	adminGot, err := s.workflowIDForViewer(req4, "billing")
	if err != nil {
		t.Fatal(err)
	}
	unscoped, _ := j.WorkflowIDBySlug(ctx, "billing")
	if adminGot != unscoped {
		t.Fatalf("admin resolved %q but the unscoped lookup says %q; admin behaviour must be unchanged", adminGot, unscoped)
	}
}
