package journal

import (
	"context"
	"fmt"
	"time"
)

// WorkerInfo is one row of the worker heartbeat registry.
type WorkerInfo struct {
	WorkerID    string    `json:"worker_id"`
	Concurrency int       `json:"concurrency"`
	LastSeen    time.Time `json:"last_seen"`
	StartedAt   time.Time `json:"started_at"`
}

// UpsertWorkerHeartbeat records (or refreshes) a worker's liveness. Called
// periodically by each `reactor worker`. concurrency is the worker's max
// in-flight runs, so the dashboard + autoscaler can reason about capacity.
func (j *Journal) UpsertWorkerHeartbeat(ctx context.Context, workerID string, concurrency int) error {
	const q = `INSERT INTO workers (worker_id, concurrency, last_seen)
		VALUES ($1, $2, $3)
		ON CONFLICT (worker_id) DO UPDATE SET concurrency = excluded.concurrency, last_seen = excluded.last_seen`
	if _, err := j.db.ExecContext(ctx, j.bind(q), workerID, concurrency, j.now()); err != nil {
		return fmt.Errorf("journal: worker heartbeat: %w", err)
	}
	return nil
}

// DeleteWorker removes a worker row on graceful shutdown so the fleet view
// + autoscaler see it leave immediately rather than after the stale window.
func (j *Journal) DeleteWorker(ctx context.Context, workerID string) error {
	if _, err := j.db.ExecContext(ctx, j.bind(`DELETE FROM workers WHERE worker_id = $1`), workerID); err != nil {
		return fmt.Errorf("journal: delete worker: %w", err)
	}
	return nil
}

// ListActiveWorkers returns workers whose heartbeat is newer than
// staleAfter ago, newest-first.
func (j *Journal) ListActiveWorkers(ctx context.Context, staleAfter time.Duration) ([]WorkerInfo, error) {
	cutoff := j.formatTime(time.Now().UTC().Add(-staleAfter))
	const q = `SELECT worker_id, concurrency, last_seen, started_at
		FROM workers WHERE last_seen >= $1 ORDER BY started_at ASC`
	rows, err := j.db.QueryContext(ctx, j.bind(q), cutoff)
	if err != nil {
		return nil, fmt.Errorf("journal: list active workers: %w", err)
	}
	defer rows.Close()
	var out []WorkerInfo
	for rows.Next() {
		var w WorkerInfo
		var lastSeen, startedAt string
		if err := rows.Scan(&w.WorkerID, &w.Concurrency, &lastSeen, &startedAt); err != nil {
			return nil, err
		}
		if t, err := j.parseTime(lastSeen); err == nil {
			w.LastSeen = t
		}
		if t, err := j.parseTime(startedAt); err == nil {
			w.StartedAt = t
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkerCapacity returns the active worker count + their summed
// concurrency (the fleet's total parallelism), for the autoscaler's
// scale decisions and the dashboard.
func (j *Journal) WorkerCapacity(ctx context.Context, staleAfter time.Duration) (count, capacity int, err error) {
	cutoff := j.formatTime(time.Now().UTC().Add(-staleAfter))
	const q = `SELECT COUNT(*), COALESCE(SUM(concurrency), 0) FROM workers WHERE last_seen >= $1`
	if err := j.db.QueryRowContext(ctx, j.bind(q), cutoff).Scan(&count, &capacity); err != nil {
		return 0, 0, fmt.Errorf("journal: worker capacity: %w", err)
	}
	return count, capacity, nil
}

// PruneStaleWorkers deletes worker rows whose heartbeat lapsed (a crashed
// worker that never DeleteWorker'd itself). Best-effort housekeeping.
func (j *Journal) PruneStaleWorkers(ctx context.Context, staleAfter time.Duration) (int64, error) {
	cutoff := j.formatTime(time.Now().UTC().Add(-staleAfter))
	res, err := j.db.ExecContext(ctx, j.bind(`DELETE FROM workers WHERE last_seen < $1`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("journal: prune stale workers: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
