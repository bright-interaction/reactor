package server

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/brightinteraction/reactor/internal/runtime/journal"
)

// plans renders the billing-plan catalog (admin-only): each tier's price,
// included allotments, caps, and overage rates, plus a create/update form.
func (s *Server) plans(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	list, err := s.Journal.ListPlans(r.Context())
	if err != nil {
		s.errorPage(w, "list plans", err)
		return
	}
	s.renderPage(w, r, page{
		Title:   "Plans",
		Heading: "Billing plans",
		Body:    template.HTML(plansBody(list, r.URL.Query().Get("err"))),
	})
}

// plansUpsert creates or updates a plan (admin-only). Money is entered in euros
// and compute in minutes for legibility; stored as cents + seconds.
func (s *Server) plansUpsert(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("plan_id"))
	if id == "" {
		http.Redirect(w, r, "/plans?err="+urlEsc("plan id is required"), http.StatusSeeOther)
		return
	}
	p := journal.Plan{
		PlanID:                     id,
		Name:                       strings.TrimSpace(r.PostFormValue("name")),
		Currency:                   strings.TrimSpace(r.PostFormValue("currency")),
		PriceCents:                 eurToCents(r.PostFormValue("price_eur")),
		IncludedExecutions:         atoiClampZero(r.PostFormValue("included_executions")),
		IncludedComputeSeconds:     int64(atoiClampZero(r.PostFormValue("included_compute_minutes"))) * 60,
		MaxConcurrentRuns:          atoiClampZero(r.PostFormValue("max_concurrent_runs")),
		MaxQueuedRuns:              atoiClampZero(r.PostFormValue("max_queued_runs")),
		OveragePer1kExecCents:      eurToCents(r.PostFormValue("overage_per_1k_exec_eur")),
		OveragePerComputeHourCents: eurToCents(r.PostFormValue("overage_per_compute_hour_eur")),
		HardCap:                    r.PostFormValue("hard_cap") == "on",
		SortOrder:                  atoiClampZero(r.PostFormValue("sort_order")),
	}
	if p.Currency == "" {
		p.Currency = "EUR"
	}
	if err := s.Journal.UpsertPlan(r.Context(), p); err != nil {
		s.errorPage(w, "save plan", err)
		return
	}
	http.Redirect(w, r, "/plans", http.StatusSeeOther)
}

// plansDelete removes a plan (admin-only). Tenants on it keep their copied
// quotas; plan_id is nulled by the FK.
func (s *Server) plansDelete(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if err := s.Journal.DeletePlan(r.Context(), chi.URLParam(r, "id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/plans", http.StatusSeeOther)
}

// eurToCents parses a euro amount ("29", "29.50") into integer cents.
func eurToCents(s string) int {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return 0
	}
	return int(math.Round(f * 100))
}

// fmtMoney renders integer cents as a currency amount.
func fmtMoney(cents int, currency string) string {
	amount := float64(cents) / 100
	if currency == "EUR" || currency == "" {
		return fmt.Sprintf("€%.2f", amount)
	}
	return fmt.Sprintf("%s %.2f", currency, amount)
}

func plansBody(list []journal.Plan, errMsg string) string {
	var b strings.Builder
	if errMsg != "" {
		b.WriteString(`<div class="err" style="background:#fff;border:1px solid var(--err);padding:12px 16px;margin:0 0 16px;border-radius:3px;">` +
			template.HTMLEscapeString(errMsg) + `</div>`)
	}
	b.WriteString(`<p class="muted">A plan is a tier. Assigning it to a tenant (on the Tenants page) copies its caps onto that tenant, so scheduling + quota enforcement use them directly. Hard-cap plans refuse runs past the included volume (no surprise bill); soft-cap plans allow overage billed from the usage ledger. Billable compute is active execution time, so suspended/waiting runs never count.</p>`)

	b.WriteString(`<table><thead><tr>` +
		`<th>Plan</th><th>Price/mo</th><th>Included</th><th>Caps</th><th>Overage</th><th>Cap mode</th><th></th>` +
		`</tr></thead><tbody>`)
	for _, p := range list {
		included := fmt.Sprintf(`%s exec<br><span class="muted">%s compute</span>`,
			fmtInt(p.IncludedExecutions), fmtCompute(float64(p.IncludedComputeSeconds)))
		caps := fmt.Sprintf(`%s concurrent<br><span class="muted">%s queued</span>`,
			quotaLabel(p.MaxConcurrentRuns), quotaLabel(p.MaxQueuedRuns))
		overage := `<span class="muted">none</span>`
		if !p.HardCap {
			overage = fmt.Sprintf(`%s / 1k exec<br><span class="muted">%s / compute-hr</span>`,
				fmtMoney(p.OveragePer1kExecCents, p.Currency), fmtMoney(p.OveragePerComputeHourCents, p.Currency))
		}
		capMode := `<span class="tag tag-on">hard cap</span>`
		if !p.HardCap {
			capMode = `<span class="tag tag-off">overage</span>`
		}
		del := fmt.Sprintf(`<form method="POST" action="/plans/%s/delete" class="form-inline" onsubmit="return confirm('Delete plan %s? Tenants on it keep their current quotas.');"><button type="submit" class="btn-link">delete</button></form>`,
			urlEsc(p.PlanID), template.JSEscapeString(p.PlanID))
		fmt.Fprintf(&b, `<tr><td><code>%s</code><br><span class="muted">%s</span></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			template.HTMLEscapeString(p.PlanID), template.HTMLEscapeString(p.Name),
			fmtMoney(p.PriceCents, p.Currency), included, caps, overage, capMode, del,
		)
	}
	b.WriteString(`</tbody></table>`)

	b.WriteString(`<h2>Create or update plan</h2>`)
	b.WriteString(`<p class="muted">Entering an existing plan id updates it and re-applies its caps to every tenant on it. Price + overage in euros; included compute in minutes.</p>`)
	b.WriteString(`<form method="POST" action="/plans" class="form">
  <label>Plan id <input type="text" name="plan_id" required autocomplete="off" placeholder="pro"></label>
  <label>Name <input type="text" name="name" autocomplete="off" placeholder="Pro"></label>
  <label>Price / month (EUR) <input type="number" name="price_eur" min="0" step="0.01" value="0"></label>
  <label>Included executions / month <input type="number" name="included_executions" min="0" value="0"></label>
  <label>Included compute (minutes) <input type="number" name="included_compute_minutes" min="0" value="0"></label>
  <label>Max concurrent runs (0 = unlimited) <input type="number" name="max_concurrent_runs" min="0" value="0"></label>
  <label>Max queued runs (0 = unlimited) <input type="number" name="max_queued_runs" min="0" value="0"></label>
  <label>Overage per 1k executions (EUR) <input type="number" name="overage_per_1k_exec_eur" min="0" step="0.01" value="0"></label>
  <label>Overage per compute-hour (EUR) <input type="number" name="overage_per_compute_hour_eur" min="0" step="0.01" value="0"></label>
  <label>Sort order <input type="number" name="sort_order" min="0" value="0"></label>
  <label class="form-inline"><input type="checkbox" name="hard_cap"> Hard cap (refuse past included volume; leave off to allow overage)</label>
  <button type="submit" class="btn-primary">Save plan</button>
</form>`)
	return b.String()
}

// fmtInt renders 0 as "unlimited" (for included allotments where 0 = no cap).
func fmtInt(n int) string {
	if n <= 0 {
		return `unlimited`
	}
	return strconv.Itoa(n)
}
