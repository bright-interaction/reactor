package journal

import (
	"context"
	"testing"
	"time"
)

func TestTenantBillOverage(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	// Tiny soft-cap plan so a few rows exceed the allotment: base 1000c,
	// 2 included execs, 2 included compute-sec, 150c per 1k exec, 40c/hour.
	if err := j.UpsertPlan(ctx, Plan{
		PlanID: "tiny", Name: "Tiny", PriceCents: 1000,
		IncludedExecutions: 2, IncludedComputeSeconds: 2,
		OveragePer1kExecCents: 150, OveragePerComputeHourCents: 40, HardCap: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := j.AssignPlan(ctx, "acme", "tiny"); err != nil {
		t.Fatal(err)
	}

	insertUsage := func(id string, active float64) {
		if _, err := j.db.ExecContext(ctx, j.bind(
			`INSERT INTO run_usage (run_id, tenant_id, workflow_id, status, run_seconds, active_seconds, step_count, recorded_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`),
			id, "acme", "wf", "succeeded", 0.0, active, 1, j.now()); err != nil {
			t.Fatal(err)
		}
	}
	// 5 runs, total active = 3602s (= 2 included + 3600 overage = 1 hour).
	insertUsage("r1", 900)
	insertUsage("r2", 900)
	insertUsage("r3", 900)
	insertUsage("r4", 900)
	insertUsage("r5", 2)

	b, err := j.TenantBill(ctx, "acme", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if b.UsedExecutions != 5 || b.OverageExecutions != 3 {
		t.Fatalf("exec overage wrong: used=%d overage=%d", b.UsedExecutions, b.OverageExecutions)
	}
	if b.OverageExecCents != 150 { // ceil(3/1000)=1 block * 150
		t.Fatalf("exec overage cents = %d, want 150", b.OverageExecCents)
	}
	if b.OverageComputeCents != 40 { // 3600s = 1.0h * 40
		t.Fatalf("compute overage cents = %d, want 40", b.OverageComputeCents)
	}
	if b.TotalCents != 1000+150+40 {
		t.Fatalf("total = %d, want 1190", b.TotalCents)
	}
}

func TestTenantBillHardCapNoOverage(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	// 'free' is hard-cap; usage past included is refused, so the bill is just
	// the base price regardless of recorded usage.
	if err := j.AssignPlan(ctx, "acme", "free"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.db.ExecContext(ctx, j.bind(
		`INSERT INTO run_usage (run_id, tenant_id, workflow_id, status, run_seconds, active_seconds, step_count, recorded_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`),
		"r1", "acme", "wf", "succeeded", 0.0, 99999.0, 1, j.now()); err != nil {
		t.Fatal(err)
	}
	b, err := j.TenantBill(ctx, "acme", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if b.OverageComputeCents != 0 || b.OverageExecCents != 0 || b.TotalCents != 0 {
		t.Fatalf("hard-cap plan should have no overage + 0 base, got %+v", b)
	}
}

func TestTenantBillNoPlan(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	// default tenant has no plan_id -> usage-only bill, no charges.
	b, err := j.TenantBill(ctx, "default", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if b.PlanID != "" || b.TotalCents != 0 {
		t.Fatalf("no-plan bill should be zero, got %+v", b)
	}
}
