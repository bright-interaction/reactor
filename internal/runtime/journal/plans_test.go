package journal

import (
	"context"
	"errors"
	"testing"
)

func TestPlanStoreSeeded(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	plans, err := j.ListPlans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 4 {
		t.Fatalf("seeded plans = %d, want 4 (free/starter/pro/scale)", len(plans))
	}
	// Ordered by sort_order: free first.
	if plans[0].PlanID != "free" || !plans[0].HardCap {
		t.Fatalf("first plan = %+v, want free + hard_cap", plans[0])
	}
	pro, err := j.GetPlan(ctx, "pro")
	if err != nil {
		t.Fatal(err)
	}
	if pro.PriceCents != 9900 || pro.MaxConcurrentRuns != 20 || pro.HardCap {
		t.Fatalf("pro plan = %+v", pro)
	}
	if _, err := j.GetPlan(ctx, "nope"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("want ErrPlanNotFound, got %v", err)
	}
}

func TestAssignPlanCopiesCaps(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	// Assign 'free' (hard cap) to a brand-new tenant: the tenant is created
	// and gets free's caps, with monthly quota = its included volume.
	if err := j.AssignPlan(ctx, "acme", "free"); err != nil {
		t.Fatal(err)
	}
	tn, err := j.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if tn.PlanID != "free" || tn.MaxConcurrentRuns != 2 || !tn.HardCap || tn.MonthlyRunQuota != 1000 {
		t.Fatalf("free assignment = %+v", tn)
	}

	// Move to 'pro' (soft cap): caps update, monthly quota becomes 0 (overage).
	if err := j.AssignPlan(ctx, "acme", "pro"); err != nil {
		t.Fatal(err)
	}
	tn, _ = j.GetTenant(ctx, "acme")
	if tn.PlanID != "pro" || tn.MaxConcurrentRuns != 20 || tn.HardCap || tn.MonthlyRunQuota != 0 {
		t.Fatalf("pro assignment = %+v", tn)
	}

	// Editing the plan re-applies to assigned tenants.
	pro, _ := j.GetPlan(ctx, "pro")
	pro.MaxConcurrentRuns = 25
	if err := j.UpsertPlan(ctx, pro); err != nil {
		t.Fatal(err)
	}
	tn, _ = j.GetTenant(ctx, "acme")
	if tn.MaxConcurrentRuns != 25 {
		t.Fatalf("plan edit did not propagate: max_concurrent = %d, want 25", tn.MaxConcurrentRuns)
	}
}
