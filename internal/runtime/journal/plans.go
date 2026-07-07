package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Plan is a billing tier. It bundles operational caps (max_concurrent,
// max_queued) with billing allotments (included executions + compute seconds)
// and overage rates. Assigning a plan to a tenant copies the caps onto the
// tenant so the existing claim + enqueue enforcement stays unchanged. Money is
// in integer minor units (cents).
type Plan struct {
	PlanID                     string
	Name                       string
	PriceCents                 int
	Currency                   string
	IncludedExecutions         int
	IncludedComputeSeconds     int64
	MaxConcurrentRuns          int
	MaxQueuedRuns              int
	OveragePer1kExecCents      int
	OveragePerComputeHourCents int
	HardCap                    bool // true: refuse past included volume; false: bill overage
	SortOrder                  int
}

// ErrPlanNotFound is returned by GetPlan for an unknown plan.
var ErrPlanNotFound = errors.New("journal: plan not found")

const planCols = `plan_id, name, price_cents, currency, included_executions, included_compute_seconds, max_concurrent_runs, max_queued_runs, overage_per_1k_exec_cents, overage_per_compute_hour_cents, hard_cap, sort_order`

func (j *Journal) scanPlan(s scanner) (Plan, error) {
	var p Plan
	var hardCap any
	if err := s.Scan(&p.PlanID, &p.Name, &p.PriceCents, &p.Currency,
		&p.IncludedExecutions, &p.IncludedComputeSeconds, &p.MaxConcurrentRuns, &p.MaxQueuedRuns,
		&p.OveragePer1kExecCents, &p.OveragePerComputeHourCents, &hardCap, &p.SortOrder); err != nil {
		return Plan{}, err
	}
	p.HardCap = parseBool(hardCap)
	return p, nil
}

// GetPlan returns one plan by id, or ErrPlanNotFound.
func (j *Journal) GetPlan(ctx context.Context, planID string) (Plan, error) {
	row := j.db.QueryRowContext(ctx, j.bind(`SELECT `+planCols+` FROM plans WHERE plan_id = $1`), planID)
	p, err := j.scanPlan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrPlanNotFound
	}
	if err != nil {
		return Plan{}, fmt.Errorf("journal: get plan: %w", err)
	}
	return p, nil
}

// ListPlans returns all plans ordered for display.
func (j *Journal) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := j.db.QueryContext(ctx, j.bind(`SELECT `+planCols+` FROM plans ORDER BY sort_order, price_cents, plan_id`))
	if err != nil {
		return nil, fmt.Errorf("journal: list plans: %w", err)
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := j.scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertPlan creates or updates a plan, then re-applies its caps to every
// tenant currently on it (so editing a plan propagates).
func (j *Journal) UpsertPlan(ctx context.Context, p Plan) error {
	if p.Currency == "" {
		p.Currency = "EUR"
	}
	const q = `INSERT INTO plans
		(plan_id, name, price_cents, currency, included_executions, included_compute_seconds,
		 max_concurrent_runs, max_queued_runs, overage_per_1k_exec_cents, overage_per_compute_hour_cents,
		 hard_cap, sort_order, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (plan_id) DO UPDATE SET
			name = excluded.name, price_cents = excluded.price_cents, currency = excluded.currency,
			included_executions = excluded.included_executions,
			included_compute_seconds = excluded.included_compute_seconds,
			max_concurrent_runs = excluded.max_concurrent_runs, max_queued_runs = excluded.max_queued_runs,
			overage_per_1k_exec_cents = excluded.overage_per_1k_exec_cents,
			overage_per_compute_hour_cents = excluded.overage_per_compute_hour_cents,
			hard_cap = excluded.hard_cap, sort_order = excluded.sort_order, updated_at = excluded.updated_at`
	if _, err := j.db.ExecContext(ctx, j.bind(q),
		p.PlanID, p.Name, p.PriceCents, p.Currency, p.IncludedExecutions, p.IncludedComputeSeconds,
		p.MaxConcurrentRuns, p.MaxQueuedRuns, p.OveragePer1kExecCents, p.OveragePerComputeHourCents,
		j.boolValue(p.HardCap), p.SortOrder, j.now()); err != nil {
		return fmt.Errorf("journal: upsert plan: %w", err)
	}
	return j.applyPlanToTenants(ctx, p)
}

// DeletePlan removes a plan. Tenants on it have plan_id set to NULL by the FK
// (their copied quotas remain until reassigned).
func (j *Journal) DeletePlan(ctx context.Context, planID string) error {
	if _, err := j.db.ExecContext(ctx, j.bind(`DELETE FROM plans WHERE plan_id = $1`), planID); err != nil {
		return fmt.Errorf("journal: delete plan: %w", err)
	}
	return nil
}

// AssignPlan puts a tenant on a plan: it records plan_id + the plan's display
// name and copies the plan's caps onto the tenant. The tenant row is created
// if missing. The monthly quota is the included execution volume on a hard-cap
// plan (refuse past it) or 0 on a soft-cap plan (bill overage instead).
func (j *Journal) AssignPlan(ctx context.Context, tenantID, planID string) error {
	p, err := j.GetPlan(ctx, planID)
	if err != nil {
		return err
	}
	monthlyQuota := planMonthlyQuota(p)
	const q = `INSERT INTO tenants
		(tenant_id, plan, plan_id, max_concurrent_runs, max_queued_runs, monthly_run_quota, hard_cap, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tenant_id) DO UPDATE SET
			plan = excluded.plan, plan_id = excluded.plan_id,
			max_concurrent_runs = excluded.max_concurrent_runs,
			max_queued_runs = excluded.max_queued_runs,
			monthly_run_quota = excluded.monthly_run_quota,
			hard_cap = excluded.hard_cap, updated_at = excluded.updated_at`
	if _, err := j.db.ExecContext(ctx, j.bind(q),
		tenantID, p.Name, p.PlanID, p.MaxConcurrentRuns, p.MaxQueuedRuns,
		monthlyQuota, j.boolValue(p.HardCap), j.now()); err != nil {
		return fmt.Errorf("journal: assign plan: %w", err)
	}
	return nil
}

// applyPlanToTenants re-applies a plan's caps to every tenant on it.
func (j *Journal) applyPlanToTenants(ctx context.Context, p Plan) error {
	const q = `UPDATE tenants SET
		plan = $1, max_concurrent_runs = $2, max_queued_runs = $3,
		monthly_run_quota = $4, hard_cap = $5, updated_at = $6
		WHERE plan_id = $7`
	if _, err := j.db.ExecContext(ctx, j.bind(q),
		p.Name, p.MaxConcurrentRuns, p.MaxQueuedRuns, planMonthlyQuota(p),
		j.boolValue(p.HardCap), j.now(), p.PlanID); err != nil {
		return fmt.Errorf("journal: apply plan to tenants: %w", err)
	}
	return nil
}

// planMonthlyQuota is the monthly run cap a plan implies: its included volume
// when hard-capped (refuse past it), else 0 (unlimited; bill overage).
func planMonthlyQuota(p Plan) int {
	if p.HardCap {
		return p.IncludedExecutions
	}
	return 0
}
