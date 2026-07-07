package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file is the pull-queue + worker-lease surface that backs Reactor's
// distributed mode. In single-node (local) mode the dispatcher executes a
// run in-process and never touches these. In distributed mode the daemon
// ENQUEUES runs (status "queued") and one or more `reactor worker`
// processes claim them atomically and execute them.
//
// The queue IS the runs table; the claim is a row lease. On Postgres the
// claim uses SELECT ... FOR UPDATE SKIP LOCKED so N workers never grab the
// same run. On SQLite (local only; distributed mode requires Postgres) the
// single-writer serialization makes the same select-then-update atomic, so
// the logic is testable without a Postgres instance.

// CreateQueuedRun records a run to be executed by a worker. Unlike
// CreateRun it leaves status "queued" and started_at NULL; a worker sets
// status "running" + started_at when it claims the run.
func (j *Journal) CreateQueuedRun(ctx context.Context, runID, workflowID, triggerKind string, triggerMeta json.RawMessage) error {
	tm := outputArg(triggerMeta, j.engine)
	// tenant_id is denormalized from the workflow so the fair-share claim and
	// quota counts read it off the run directly (workflowID passed twice
	// because the $N rewriter does not dedupe repeated params).
	const q = `INSERT INTO runs (id, workflow_id, trigger_kind, trigger_meta, status, created_at, tenant_id)
		VALUES ($1, $2, $3, $4, 'queued', $5, COALESCE((SELECT tenant_id FROM workflows WHERE id = $6), 'default'))`
	if _, err := j.db.ExecContext(ctx, j.bind(q), runID, workflowID, triggerKind, tm, j.now(), workflowID); err != nil {
		return fmt.Errorf("journal: create queued run: %w", err)
	}
	return nil
}

// ClaimQueuedRuns atomically leases up to limit queued runs for workerID,
// flips them to "running", and writes a lease row per run. Returns the
// claimed run ids (possibly empty). leaseTTL is how long the claim is
// valid before ReapExpiredLeases may requeue the run (a worker extends its
// lease via ExtendLease while executing).
func (j *Journal) ClaimQueuedRuns(ctx context.Context, workerID string, limit int, leaseTTL time.Duration) ([]string, error) {
	if limit <= 0 {
		limit = 1
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Pick candidates tenant-fairly (interleaved by per-tenant queue position,
	// disabled tenants skipped, over-concurrency tenants dropped). This select
	// does not lock -- it uses window functions, which Postgres forbids
	// combining with FOR UPDATE -- so on Postgres we lock the chosen subset in
	// a second pass. Fetch more candidates than needed so SKIP LOCKED
	// contention between workers does not starve a claim.
	candLimit := limit * 4
	if candLimit < 8 {
		candLimit = 8
	}
	candidates, err := scanIDs(tx.QueryContext(ctx, j.bind(j.fairCandidateQuery(candLimit))))
	if err != nil {
		return nil, fmt.Errorf("journal: claim candidate select: %w", err)
	}
	if len(candidates) == 0 {
		return nil, tx.Commit()
	}

	var ids []string
	if j.engine == EnginePostgres {
		// Lock the still-queued, unlocked subset; SKIP LOCKED makes concurrent
		// claimers grab disjoint rows. Then re-impose the fair order (IN does
		// not preserve it) and take up to limit.
		ph := make([]string, len(candidates))
		args := make([]any, len(candidates))
		for i, id := range candidates {
			ph[i] = "$" + strconv.Itoa(i+1)
			args[i] = id
		}
		lockQ := `SELECT id FROM runs WHERE status = 'queued' AND id IN (` +
			strings.Join(ph, ",") + `) FOR UPDATE SKIP LOCKED`
		locked, lerr := scanIDs(tx.QueryContext(ctx, lockQ, args...))
		if lerr != nil {
			return nil, fmt.Errorf("journal: claim lock: %w", lerr)
		}
		lockedSet := make(map[string]bool, len(locked))
		for _, id := range locked {
			lockedSet[id] = true
		}
		for _, id := range candidates {
			if lockedSet[id] {
				ids = append(ids, id)
				if len(ids) == limit {
					break
				}
			}
		}
	} else {
		// SQLite serializes writers, so the candidates are already safe to
		// claim; just take up to limit in fair order.
		if len(candidates) > limit {
			candidates = candidates[:limit]
		}
		ids = candidates
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}

	expires := j.formatTime(time.Now().UTC().Add(leaseTTL))
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			j.bind(`UPDATE runs SET status = 'running', started_at = $1 WHERE id = $2`),
			j.now(), id); err != nil {
			return nil, fmt.Errorf("journal: claim mark running: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			j.bind(`INSERT INTO leases (run_id, worker_id, expires_at) VALUES ($1, $2, $3)
				ON CONFLICT (run_id) DO UPDATE SET worker_id = excluded.worker_id, expires_at = excluded.expires_at`),
			id, workerID, expires); err != nil {
			return nil, fmt.Errorf("journal: claim lease insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ExtendLease renews the lease on a run the worker is still executing.
// Only the owning worker can extend, so a reaped+reclaimed run isn't
// renewed by its dead original owner.
func (j *Journal) ExtendLease(ctx context.Context, runID, workerID string, ttl time.Duration) error {
	const q = `UPDATE leases SET expires_at = $1 WHERE run_id = $2 AND worker_id = $3`
	_, err := j.db.ExecContext(ctx, j.bind(q),
		j.formatTime(time.Now().UTC().Add(ttl)), runID, workerID)
	if err != nil {
		return fmt.Errorf("journal: extend lease: %w", err)
	}
	return nil
}

// ReleaseLease drops a run's lease. Called when the run reaches a terminal
// status so the row no longer counts as in-flight.
func (j *Journal) ReleaseLease(ctx context.Context, runID string) error {
	if _, err := j.db.ExecContext(ctx, j.bind(`DELETE FROM leases WHERE run_id = $1`), runID); err != nil {
		return fmt.Errorf("journal: release lease: %w", err)
	}
	return nil
}

// ReapExpiredLeases requeues runs whose worker died (lease expired while
// the run was still "running") and drops the stale leases. Re-running is
// safe: the supervisor replays completed steps from the journal cache, so
// a requeued run resumes rather than repeating side effects. Returns the
// number of runs requeued.
func (j *Journal) ReapExpiredLeases(ctx context.Context) (int64, error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := j.formatTime(time.Now().UTC())
	res, err := tx.ExecContext(ctx,
		j.bind(`UPDATE runs SET status = 'queued'
			WHERE status = 'running'
			AND id IN (SELECT run_id FROM leases WHERE expires_at < $1)`),
		now)
	if err != nil {
		return 0, fmt.Errorf("journal: reap requeue: %w", err)
	}
	if _, err := tx.ExecContext(ctx, j.bind(`DELETE FROM leases WHERE expires_at < $1`), now); err != nil {
		return 0, fmt.Errorf("journal: reap delete leases: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// fairCandidateQuery builds the tenant-fair, quota-bounded candidate select
// (no locking; the Postgres claim locks the result separately because window
// functions cannot combine with FOR UPDATE). It:
//   - interleaves tenants by per-tenant queue position (ROW_NUMBER partitioned
//     by tenant ordered by created_at), so all tenants' oldest run is offered
//     before any tenant's second -- one tenant's backlog cannot starve others;
//   - skips runs of disabled tenants;
//   - drops runs that would exceed a tenant's max_concurrent_runs (the run's
//     position rn must fit within cap minus currently-running). A tenant with
//     no tenants row, or a 0/NULL cap, is unlimited + enabled.
//
// The concurrency cap is enforced best-effort: each claimer reads the running
// count once, so concurrent claimers can briefly overshoot by up to a batch.
// Fair interleaving (not the hard cap) is what prevents starvation.
func (j *Journal) fairCandidateQuery(limit int) string {
	falseLit := "false"
	if j.engine == EngineSQLite {
		falseLit = "0"
	}
	return `WITH running AS (
		SELECT tenant_id, COUNT(*) AS n FROM runs WHERE status = 'running' GROUP BY tenant_id
	),
	ranked AS (
		SELECT id, tenant_id, created_at,
			ROW_NUMBER() OVER (PARTITION BY tenant_id ORDER BY created_at, id) AS rn
		FROM runs WHERE status = 'queued'
	)
	SELECT k.id
	FROM ranked k
	LEFT JOIN tenants t ON t.tenant_id = k.tenant_id
	LEFT JOIN running ru ON ru.tenant_id = k.tenant_id
	WHERE (t.disabled IS NULL OR t.disabled = ` + falseLit + `)
		AND (t.max_concurrent_runs IS NULL OR t.max_concurrent_runs <= 0
			OR k.rn <= t.max_concurrent_runs - COALESCE(ru.n, 0))
	ORDER BY k.rn, k.created_at, k.id
	LIMIT ` + strconv.Itoa(limit)
}

// scanIDs collects a single-column id result set, closing rows.
func scanIDs(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountQueued returns how many runs are waiting to be claimed. Powers the
// worker autoscaler's scale-up signal + a /metrics gauge.
func (j *Journal) CountQueued(ctx context.Context) (int, error) {
	var n int
	if err := j.db.QueryRowContext(ctx, j.bind(`SELECT COUNT(*) FROM runs WHERE status = 'queued'`)).Scan(&n); err != nil {
		return 0, fmt.Errorf("journal: count queued: %w", err)
	}
	return n, nil
}
