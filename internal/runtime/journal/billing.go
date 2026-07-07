package journal

import (
	"context"
	"errors"
	"math"
	"time"
)

// TenantBill is a tenant's current-period charge estimate: the plan base fee
// plus metered overage above the included allotments. All money is integer
// cents. On a hard-cap plan there is no overage (usage is refused past the
// quota), so TotalCents is just the base price.
type TenantBill struct {
	TenantID               string  `json:"tenant_id"`
	PlanID                 string  `json:"plan_id"`
	PlanName               string  `json:"plan_name"`
	Currency               string  `json:"currency"`
	HardCap                bool    `json:"hard_cap"`
	BasePriceCents         int     `json:"base_price_cents"`
	IncludedExecutions     int     `json:"included_executions"`
	IncludedComputeSeconds int64   `json:"included_compute_seconds"`
	UsedExecutions         int     `json:"used_executions"`
	UsedComputeSeconds     float64 `json:"used_compute_seconds"`
	OverageExecutions      int     `json:"overage_executions"`
	OverageComputeSeconds  float64 `json:"overage_compute_seconds"`
	OverageExecCents       int     `json:"overage_exec_cents"`
	OverageComputeCents    int     `json:"overage_compute_cents"`
	TotalCents             int     `json:"total_cents"`
}

// TenantBill computes a tenant's charge estimate for the period beginning at
// since (pass the start of the billing month). A tenant with no plan returns a
// usage-only bill (zero charges).
func (j *Journal) TenantBill(ctx context.Context, tenantID string, since time.Time) (TenantBill, error) {
	usage, err := j.TenantUsageSince(ctx, tenantID, since)
	if err != nil {
		return TenantBill{}, err
	}
	b := TenantBill{
		TenantID:           tenantID,
		Currency:           "EUR",
		UsedExecutions:     usage.Runs,
		UsedComputeSeconds: usage.ActiveSeconds,
	}
	tn, err := j.GetTenant(ctx, tenantID)
	if errors.Is(err, ErrTenantNotFound) || tn.PlanID == "" {
		return b, nil // no plan: usage only, nothing to bill
	}
	if err != nil {
		return TenantBill{}, err
	}
	p, err := j.GetPlan(ctx, tn.PlanID)
	if errors.Is(err, ErrPlanNotFound) {
		return b, nil // plan was deleted; treat as no plan
	}
	if err != nil {
		return TenantBill{}, err
	}

	b.PlanID = p.PlanID
	b.PlanName = p.Name
	b.Currency = p.Currency
	b.HardCap = p.HardCap
	b.BasePriceCents = p.PriceCents
	b.IncludedExecutions = p.IncludedExecutions
	b.IncludedComputeSeconds = p.IncludedComputeSeconds
	b.TotalCents = p.PriceCents

	// Hard-cap plans never overage (excess usage is refused at enqueue).
	if p.HardCap {
		return b, nil
	}
	// Execution overage, billed per started 1,000 block.
	if p.IncludedExecutions > 0 && usage.Runs > p.IncludedExecutions {
		b.OverageExecutions = usage.Runs - p.IncludedExecutions
		blocks := math.Ceil(float64(b.OverageExecutions) / 1000.0)
		b.OverageExecCents = int(blocks * float64(p.OveragePer1kExecCents))
	}
	// Compute overage, billed fractionally per hour.
	if p.IncludedComputeSeconds > 0 && usage.ActiveSeconds > float64(p.IncludedComputeSeconds) {
		b.OverageComputeSeconds = usage.ActiveSeconds - float64(p.IncludedComputeSeconds)
		hours := b.OverageComputeSeconds / 3600.0
		b.OverageComputeCents = int(math.Round(hours * float64(p.OveragePerComputeHourCents)))
	}
	b.TotalCents += b.OverageExecCents + b.OverageComputeCents
	return b, nil
}
