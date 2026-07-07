package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brightinteraction/reactor/internal/auth"
)

// TestDashboardTenantIsolation is the security regression for the SaaS
// dashboard: a member sees only their own tenant's runs, and cannot open
// another tenant's run (404, not 403, so existence isn't leaked). Admins see
// everything.
func TestDashboardTenantIsolation(t *testing.T) {
	t.Parallel()
	j := journalForServerTest(t)
	s := &Server{Journal: j, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	// A default-tenant workflow + run.
	if err := j.CreateWorkflow(ctx, "wf_d", "demo", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateRun(ctx, "run_d", "wf_d", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	listBody := func(u *auth.User) string {
		req := httptest.NewRequest(http.MethodGet, "/runs", nil)
		if u != nil {
			req = req.WithContext(withUser(req.Context(), *u))
		}
		rec := httptest.NewRecorder()
		s.runs(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/runs code = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}
	detailCode := func(u *auth.User) int {
		req := httptest.NewRequest(http.MethodGet, "/runs/run_d", nil)
		if u != nil {
			req = req.WithContext(withUser(req.Context(), *u))
		}
		req = withChiParam(req, "id", "run_d")
		rec := httptest.NewRecorder()
		s.runDetail(rec, req)
		return rec.Code
	}

	admin := &auth.User{ID: "a", Role: auth.RoleAdmin, TenantID: "default"}
	memberDefault := &auth.User{ID: "m1", Role: auth.RoleMember, TenantID: "default"}
	memberOther := &auth.User{ID: "m2", Role: auth.RoleMember, TenantID: "acme"}

	// Admin + same-tenant member see the run.
	if !strings.Contains(listBody(admin), "run_d") {
		t.Fatal("admin should see run_d in /runs")
	}
	if !strings.Contains(listBody(memberDefault), "run_d") {
		t.Fatal("default-tenant member should see run_d")
	}
	if code := detailCode(memberDefault); code != http.StatusOK {
		t.Fatalf("same-tenant member runDetail = %d, want 200", code)
	}

	// A member of another tenant sees nothing and cannot open the run.
	if strings.Contains(listBody(memberOther), "run_d") {
		t.Fatal("ISOLATION LEAK: other-tenant member saw run_d in /runs")
	}
	if code := detailCode(memberOther); code != http.StatusNotFound {
		t.Fatalf("ISOLATION LEAK: other-tenant member runDetail = %d, want 404", code)
	}
	// Admin can open it.
	if code := detailCode(admin); code != http.StatusOK {
		t.Fatalf("admin runDetail = %d, want 200", code)
	}
}
