package server

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// fmtInt2 renders an integer with thousands separators (readable usage counts).
func fmtInt2(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// account is the customer-facing, read-only view of the signed-in viewer's own
// tenant: plan, this-month usage against the included allotments, estimated
// bill, and live activity. Any authenticated user sees their own tenant (the
// read-only counterpart to the admin Tenants page).
func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	tenantID := "default"
	if u, ok := UserFromContext(r.Context()); ok && u.TenantID != "" {
		tenantID = u.TenantID
	}
	ctx := r.Context()
	since := journal.StartOfMonthUTC()
	tn, _ := s.Journal.GetTenant(ctx, tenantID)
	usage, _ := s.Journal.TenantUsageSince(ctx, tenantID, since)
	bill, _ := s.Journal.TenantBill(ctx, tenantID, since)
	running, _ := s.Journal.CountRunningForTenant(ctx, tenantID)
	queued, _ := s.Journal.CountQueuedForTenant(ctx, tenantID)
	username := ""
	if u, ok := UserFromContext(r.Context()); ok {
		username = u.Username
	}
	body := accountBody(tenantID, tn, usage, bill, running, queued) +
		accountPasswordForm(s.Auth != nil && username != "", username)
	s.renderPage(w, r, page{
		Title:   "Account",
		Heading: "Your account",
		Body:    template.HTML(body),
	})
}

func accountBody(tenantID string, tn journal.Tenant, usage journal.TenantUsage, bill journal.TenantBill, running, queued int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<p class="muted">Tenant <code>%s</code>. Usage + bill are for the current calendar month. Billable compute is active execution time, so workflows that are waiting/suspended cost nothing while they wait.</p>`,
		template.HTMLEscapeString(tenantID))

	// Plan + estimated bill.
	plan := `<span class="muted">no plan assigned</span>`
	if bill.PlanID != "" {
		plan = fmt.Sprintf(`<code>%s</code> &middot; %s/mo`,
			template.HTMLEscapeString(bill.PlanID), template.HTMLEscapeString(fmtMoney(bill.BasePriceCents, bill.Currency)))
	}
	b.WriteString(`<div class="analytics-detail" open><summary>Plan</summary>`)
	fmt.Fprintf(&b, `<p>%s</p>`, plan)
	if bill.PlanID != "" {
		fmt.Fprintf(&b, `<p><strong>Estimated this month: %s</strong>`, template.HTMLEscapeString(fmtMoney(bill.TotalCents, bill.Currency)))
		if bill.OverageExecCents > 0 || bill.OverageComputeCents > 0 {
			fmt.Fprintf(&b, ` <span class="muted">(base %s + overage %s)</span>`,
				template.HTMLEscapeString(fmtMoney(bill.BasePriceCents, bill.Currency)),
				template.HTMLEscapeString(fmtMoney(bill.OverageExecCents+bill.OverageComputeCents, bill.Currency)))
		}
		b.WriteString(`</p>`)
	}
	b.WriteString(`</div>`)

	// Usage vs included.
	b.WriteString(`<table><thead><tr><th>Metric</th><th>Used this month</th><th>Included</th></tr></thead><tbody>`)
	fmt.Fprintf(&b, `<tr><td>Executions</td><td>%s</td><td>%s</td></tr>`,
		fmtInt2(usage.Runs), includedExec(bill))
	fmt.Fprintf(&b, `<tr><td>Active compute</td><td>%s</td><td>%s</td></tr>`,
		fmtCompute(usage.ActiveSeconds), includedCompute(bill))
	fmt.Fprintf(&b, `<tr><td>Steps</td><td>%s</td><td class="muted">-</td></tr>`, fmtInt2(usage.Steps))
	fmt.Fprintf(&b, `<tr><td>Running now</td><td>%d</td><td class="muted">cap %s</td></tr>`, running, quotaLabel(tn.MaxConcurrentRuns))
	fmt.Fprintf(&b, `<tr><td>Queued now</td><td>%d</td><td class="muted">-</td></tr>`, queued)
	b.WriteString(`</tbody></table>`)
	b.WriteString(`<p class="muted">Need a higher plan or a quota change? Contact your administrator.</p>`)
	return b.String()
}

func includedExec(bill journal.TenantBill) string {
	if bill.PlanID == "" || bill.IncludedExecutions <= 0 {
		return `<span class="muted">-</span>`
	}
	return fmtInt2(bill.IncludedExecutions)
}

func includedCompute(bill journal.TenantBill) string {
	if bill.PlanID == "" || bill.IncludedComputeSeconds <= 0 {
		return `<span class="muted">-</span>`
	}
	return fmtCompute(float64(bill.IncludedComputeSeconds))
}
