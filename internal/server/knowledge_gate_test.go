package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/reactor/internal/auth"
	"github.com/bright-interaction/reactor/internal/knowledge"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// knowledgeFixture writes one post-mortem entry naming another tenant's
// workflow, step and error text -- exactly what the generator emits from a
// failed run (see internal/postmortem).
func knowledgeFixture(t *testing.T) *knowledge.Store {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "post-mortems"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `---
id: km_1
topic: post-mortems
title: acme-payroll run failed
created_by: reactor
---

Root cause: workflow acme-payroll step charge-card failed with
err="stripe 402 card_declined for customer cus_TENANTB_SECRET"
`
	if err := os.WriteFile(filepath.Join(root, "post-mortems", "km_1.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ks, err := knowledge.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

// TestKnowledgeReadsAreAdminGated pins a cross-tenant read leak.
//
// /postmortems gates on requireAdmin, but /knowledge and /knowledge/{id} serve
// the SAME corpus and were mounted on the member router. internal/knowledge has
// no tenant concept at all (no column, no filter, no parameter), and s.knowledge
// calls List(ctx, "") with no topic filter, so the member view included topic
// "post-mortems" -- whose bodies carry another tenant's workflow slug, step
// names and truncated step error text.
//
// Same shape as the /graph.json-vs-query_graph inconsistency: identical data,
// two mount points, one gate. Authorization in Mount is POSITIONAL, so asserting
// on middleware depth catches a future move that reading the source would not.
func TestKnowledgeReadsAreAdminGated(t *testing.T) {
	t.Parallel()
	s := &Server{
		Journal:   &journal.Journal{},
		Knowledge: knowledgeFixture(t),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r := chi.NewRouter()
	s.Mount(r)

	depth := map[string]int{}
	if err := chi.Walk(r, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		depth[method+" "+route] = len(mws)
		return nil
	}); err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	memberDepth, ok := depth["GET /runs"]
	if !ok {
		t.Fatal("/runs is not registered; it is the member reference route")
	}
	for _, route := range []string{"GET /knowledge", "GET /knowledge/{id}"} {
		d, ok := depth[route]
		if !ok {
			t.Fatalf("%s is not registered; wire s.Knowledge in this test", route)
		}
		if d <= memberDepth {
			t.Fatalf("%s middleware depth %d must exceed the member-route depth %d (it serves the post-mortems corpus, which /postmortems already admin-gates)", route, d, memberDepth)
		}
	}
}

// TestKnowledgeDoesNotLeakPostmortemsToMembers is the functional half: a member
// must not be able to read another tenant's post-mortem body through the
// knowledge corpus.
func TestKnowledgeDoesNotLeakPostmortemsToMembers(t *testing.T) {
	t.Parallel()
	ks := knowledgeFixture(t)
	s := &Server{Knowledge: ks, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Sanity: the corpus really holds the post-mortem, so a pass cannot come
	// from an empty fixture.
	if entries, err := ks.List(context.Background(), ""); err != nil || len(entries) == 0 {
		t.Fatalf("fixture not loaded: entries=%d err=%v", len(entries), err)
	}

	member := auth.User{ID: "m", Role: auth.RoleMember, TenantID: "tenant-a"}
	req := httptest.NewRequest(http.MethodGet, "/knowledge", nil)
	req = req.WithContext(withUser(req.Context(), member))
	rec := httptest.NewRecorder()
	s.knowledge(rec, req)

	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "acme-payroll") {
		t.Fatalf("member (tenant-a) read another tenant's post-mortem through /knowledge (%d); /postmortems correctly refuses the same data", rec.Code)
	}
}

// TestKnowledgeNavIsAdminOnly keeps the nav honest: a member must not be shown
// a link they will only get a 403 from.
func TestKnowledgeNavIsAdminOnly(t *testing.T) {
	t.Parallel()
	for _, it := range navItems {
		if it.href == "/knowledge" {
			if !it.adminOnly {
				t.Fatal("/knowledge is in the nav as member-visible but its routes are admin-gated; members would see a link that 403s")
			}
			return
		}
	}
	t.Fatal("/knowledge is missing from navItems")
}
