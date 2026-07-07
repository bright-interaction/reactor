package journal

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

// Analytics is the home-dashboard rollup the operator sees on first
// load. Counts by terminal status, duration percentiles over succeeded
// runs (so failed-DLQ outliers do not skew the perf number), and
// total minutes saved (sum of estimated_minutes_saved_per_run per
// workflow times that workflow's succeeded count).
type Analytics struct {
	// TotalRuns includes every status (running, succeeded, failed,
	// failed_dlq, suspended). RunsByStatus breaks down the counts.
	TotalRuns     int
	RunsByStatus  map[string]int
	SucceededRuns int

	// AvgDurationMs and P95DurationMs are computed over succeeded runs
	// only. Failed / dead-lettered runs are excluded so a hostile or
	// buggy workflow's failure pattern doesn't poison the perf headline.
	AvgDurationMs int64
	P95DurationMs int64

	// TotalMinutesSaved is the headline operator number: sum across
	// workflows of (estimated_minutes_saved_per_run * succeeded_runs).
	// Workflows with a zero baseline contribute nothing.
	TotalMinutesSaved int64

	// PerWorkflow is the per-workflow breakdown sorted by minutes saved
	// descending so the highest-impact automations land at the top of
	// the dashboard table.
	PerWorkflow []WorkflowAnalytics

	// DailyRuns is a 7-element slice covering the last 7 days
	// (DailyRuns[0] is six days ago, DailyRuns[6] is today). Drives the
	// sparkline-style activity strip on the home page.
	DailyRuns []DailyRunCount
}

// WorkflowAnalytics is the per-workflow rollup row.
type WorkflowAnalytics struct {
	WorkflowID                  string
	Slug                        string
	SucceededRuns               int
	FailedRuns                  int
	AvgDurationMs               int64
	EstimatedMinutesSavedPerRun int
	MinutesSavedTotal           int64
}

// DailyRunCount is one bucket in the 7-day strip.
type DailyRunCount struct {
	Day   time.Time // 00:00 UTC of the bucket
	Total int
}

// AnalyticsSummary returns the home-dashboard rollup. A single read
// against the journal: ListWorkflows + a few aggregating queries. The
// home handler caches nothing; queries are O(workflows) + O(runs) but
// runs is bounded by recent activity.
func (j *Journal) AnalyticsSummary(ctx context.Context) (Analytics, error) {
	wfs, err := j.ListWorkflows(ctx)
	if err != nil {
		return Analytics{}, fmt.Errorf("analytics: list workflows: %w", err)
	}

	statusCounts, err := j.countRunsByStatus(ctx)
	if err != nil {
		return Analytics{}, err
	}

	out := Analytics{
		RunsByStatus: statusCounts,
		DailyRuns:    make([]DailyRunCount, 7),
	}
	for _, n := range statusCounts {
		out.TotalRuns += n
	}
	out.SucceededRuns = statusCounts[StatusSucceeded]

	// Per-workflow succeeded count + avg duration. One query per
	// workflow is fine for the v1 dashboard (workflow count is
	// typically dozens, not thousands).
	out.PerWorkflow = make([]WorkflowAnalytics, 0, len(wfs))
	for _, w := range wfs {
		row, perr := j.workflowAnalytics(ctx, w)
		if perr != nil {
			return Analytics{}, perr
		}
		out.PerWorkflow = append(out.PerWorkflow, row)
		out.TotalMinutesSaved += row.MinutesSavedTotal
	}

	// Global avg + p95 over every succeeded run.
	durations, err := j.successfulRunDurationsMs(ctx)
	if err != nil {
		return Analytics{}, err
	}
	out.AvgDurationMs = averageMs(durations)
	out.P95DurationMs = percentileMs(durations, 0.95)

	sort.Slice(out.PerWorkflow, func(i, j int) bool {
		if out.PerWorkflow[i].MinutesSavedTotal != out.PerWorkflow[j].MinutesSavedTotal {
			return out.PerWorkflow[i].MinutesSavedTotal > out.PerWorkflow[j].MinutesSavedTotal
		}
		// Tiebreak by succeeded count so workflows with no declared
		// baseline still surface in a stable order.
		return out.PerWorkflow[i].SucceededRuns > out.PerWorkflow[j].SucceededRuns
	})

	// Last-7-day activity strip. Anchor to UTC midnight so the bucket
	// boundaries are deterministic across daemon timezones.
	daily, err := j.recentDailyRunCounts(ctx, 7)
	if err != nil {
		return Analytics{}, err
	}
	out.DailyRuns = daily

	return out, nil
}

// countRunsByStatus runs one GROUP BY query for the whole runs table.
func (j *Journal) countRunsByStatus(ctx context.Context) (map[string]int, error) {
	const q = `SELECT status, COUNT(*) FROM runs GROUP BY status`
	rows, err := j.db.QueryContext(ctx, j.bind(q))
	if err != nil {
		return nil, fmt.Errorf("analytics: status counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("analytics: scan status: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}

// workflowAnalytics fetches succeeded + failed counts and an average
// duration in one round-trip per workflow.
func (j *Journal) workflowAnalytics(ctx context.Context, w Workflow) (WorkflowAnalytics, error) {
	row := WorkflowAnalytics{
		WorkflowID:                  w.ID,
		Slug:                        w.Slug,
		EstimatedMinutesSavedPerRun: w.EstimatedMinutesSavedPerRun,
	}
	// Count succeeded vs failed-class. failed_dlq counts as failed for
	// the operator's headline rollup.
	const countQ = `SELECT status, COUNT(*) FROM runs WHERE workflow_id = $1 GROUP BY status`
	rows, err := j.db.QueryContext(ctx, j.bind(countQ), w.ID)
	if err != nil {
		return WorkflowAnalytics{}, fmt.Errorf("analytics: workflow %s counts: %w", w.Slug, err)
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return WorkflowAnalytics{}, err
		}
		switch status {
		case StatusSucceeded:
			row.SucceededRuns = n
		case "failed", "failed_dlq":
			row.FailedRuns += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return WorkflowAnalytics{}, err
	}

	// Avg duration via the journal (covers both sqlite + postgres time
	// formats so we compute in Go after fetching).
	durations, derr := j.successfulRunDurationsForWorkflowMs(ctx, w.ID)
	if derr != nil {
		return WorkflowAnalytics{}, derr
	}
	row.AvgDurationMs = averageMs(durations)
	row.MinutesSavedTotal = int64(row.EstimatedMinutesSavedPerRun) * int64(row.SucceededRuns)
	return row, nil
}

// successfulRunDurationsMs returns durations (ms) for every succeeded
// run with both timestamps populated. Computed in Go so the SQL is
// engine-portable; the result set is the number of succeeded runs
// total, which is fine for the v1 dashboard (the operator dashboard
// expects O(thousands), not millions).
func (j *Journal) successfulRunDurationsMs(ctx context.Context) ([]int64, error) {
	const q = `SELECT started_at, finished_at FROM runs
		WHERE status = $1 AND started_at IS NOT NULL AND finished_at IS NOT NULL`
	return j.scanDurations(ctx, q, StatusSucceeded)
}

func (j *Journal) successfulRunDurationsForWorkflowMs(ctx context.Context, workflowID string) ([]int64, error) {
	const q = `SELECT started_at, finished_at FROM runs
		WHERE status = $1 AND workflow_id = $2 AND started_at IS NOT NULL AND finished_at IS NOT NULL`
	return j.scanDurations(ctx, q, StatusSucceeded, workflowID)
}

func (j *Journal) scanDurations(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := j.db.QueryContext(ctx, j.bind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: durations: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var startedRaw, finishedRaw sql.NullString
		if err := rows.Scan(&startedRaw, &finishedRaw); err != nil {
			return nil, err
		}
		if !startedRaw.Valid || !finishedRaw.Valid {
			continue
		}
		started, err := j.parseTime(startedRaw.String)
		if err != nil {
			continue
		}
		finished, err := j.parseTime(finishedRaw.String)
		if err != nil {
			continue
		}
		ms := finished.Sub(started).Milliseconds()
		if ms < 0 {
			continue
		}
		out = append(out, ms)
	}
	return out, rows.Err()
}

// recentDailyRunCounts returns the last n days of run counts anchored
// to UTC midnight. Computes buckets in Go for SQLite + Postgres
// portability; SELECTing every row from the last n days is cheap
// because the runs table is already indexed on started_at.
func (j *Journal) recentDailyRunCounts(ctx context.Context, n int) ([]DailyRunCount, error) {
	if n <= 0 {
		return nil, nil
	}
	nowT := time.Now().UTC()
	today := time.Date(nowT.Year(), nowT.Month(), nowT.Day(), 0, 0, 0, 0, time.UTC)
	cutoff := today.AddDate(0, 0, -(n - 1))

	const q = `SELECT started_at FROM runs WHERE started_at IS NOT NULL AND started_at >= $1`
	rows, err := j.db.QueryContext(ctx, j.bind(q), j.formatTime(cutoff))
	if err != nil {
		return nil, fmt.Errorf("analytics: daily counts: %w", err)
	}
	defer rows.Close()
	buckets := make([]DailyRunCount, n)
	for i := range buckets {
		buckets[i] = DailyRunCount{Day: cutoff.AddDate(0, 0, i)}
	}
	for rows.Next() {
		var startedRaw sql.NullString
		if err := rows.Scan(&startedRaw); err != nil {
			return nil, err
		}
		if !startedRaw.Valid {
			continue
		}
		started, err := j.parseTime(startedRaw.String)
		if err != nil {
			continue
		}
		dayStart := time.Date(started.Year(), started.Month(), started.Day(), 0, 0, 0, 0, time.UTC)
		idx := int(dayStart.Sub(cutoff).Hours()/24 + 0.5)
		if idx < 0 || idx >= n {
			continue
		}
		buckets[idx].Total++
	}
	return buckets, rows.Err()
}

func averageMs(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	var sum int64
	for _, v := range values {
		sum += v
	}
	return sum / int64(len(values))
}

// percentileMs returns the rank-p value (e.g. p=0.95 -> p95). Uses the
// nearest-rank method which is simple, deterministic, and adequate for
// the dashboard summary (a real APM would interpolate).
func percentileMs(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int64, len(values))
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// Nearest-rank: rank = ceil(p*N), 1-based, so the 0-based index is
	// ceil(p*N)-1. The old int(p*N) floored and indexed 0-based, which
	// overshot by one (p95 of 1..100 returned 96 instead of 95).
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
