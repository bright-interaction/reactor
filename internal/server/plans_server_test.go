package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/brightinteraction/reactor/internal/auth"
)

func TestPlansPageAndAssign(t *testing.T) {
	t.Parallel()
	j := journalForServerTest(t)
	s := &Server{Journal: j, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	admin := auth.User{ID: "a1", Role: auth.RoleAdmin}

	// Plans page renders the seeded tiers.
	greq := httptest.NewRequest(http.MethodGet, "/plans", nil).WithContext(withUser(context.Background(), admin))
	grec := httptest.NewRecorder()
	s.plans(grec, greq)
	if grec.Code != http.StatusOK || !strings.Contains(grec.Body.String(), "pro") {
		t.Fatalf("plans page code=%d, missing pro tier", grec.Code)
	}

	// Create a custom plan via the form (euros -> cents, minutes -> seconds).
	form := url.Values{
		"plan_id": {"team"}, "name": {"Team"}, "price_eur": {"49.50"},
		"included_executions": {"50000"}, "included_compute_minutes": {"5000"},
		"max_concurrent_runs": {"10"}, "overage_per_1k_exec_eur": {"1.50"},
	}
	preq := httptest.NewRequest(http.MethodPost, "/plans", strings.NewReader(form.Encode()))
	preq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	preq = preq.WithContext(withUser(preq.Context(), admin))
	prec := httptest.NewRecorder()
	s.plansUpsert(prec, preq)
	if prec.Code != http.StatusSeeOther {
		t.Fatalf("plan upsert code=%d, want 303", prec.Code)
	}
	p, err := j.GetPlan(context.Background(), "team")
	if err != nil || p.PriceCents != 4950 || p.IncludedComputeSeconds != 300000 || p.MaxConcurrentRuns != 10 {
		t.Fatalf("plan not persisted with unit conversion: %+v err=%v", p, err)
	}

	// Assign it to a tenant; the plan's caps copy onto the tenant.
	areq := httptest.NewRequest(http.MethodPost, "/tenants/acme/plan",
		strings.NewReader(url.Values{"plan_id": {"team"}}.Encode()))
	areq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	areq = areq.WithContext(withUser(areq.Context(), admin))
	areq = withChiParam(areq, "id", "acme")
	arec := httptest.NewRecorder()
	s.tenantsAssignPlan(arec, areq)
	if arec.Code != http.StatusSeeOther {
		t.Fatalf("assign code=%d, want 303", arec.Code)
	}
	tn, err := j.GetTenant(context.Background(), "acme")
	if err != nil || tn.PlanID != "team" || tn.MaxConcurrentRuns != 10 {
		t.Fatalf("plan not applied to tenant: %+v err=%v", tn, err)
	}
}
