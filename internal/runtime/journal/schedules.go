package journal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Schedule represents a pending wake-up: workflow subprocess was suspended
// at a Sleep or AwaitSignal boundary, the host kept the run alive, and a
// scheduler tick will re-spawn it at wake_at.
type Schedule struct {
	ID            string
	RunID         string
	StepName      string
	Kind          string // sleep | signal | cron
	WakeAt        time.Time
	SignalName    string
	SignalToken   string
	SignalPayload []byte
	Fired         bool
	CreatedAt     time.Time
}

// ScheduleKind values.
const (
	KindSleep  = "sleep"
	KindSignal = "signal"
	KindCron   = "cron"
)

// ErrAlreadyFired is returned by FireSignal when the token's schedule row
// has already been delivered. Callers map this to HTTP 410 Gone.
var ErrAlreadyFired = errors.New("journal: schedule already fired")

// ScheduleSleep persists a sleep schedule. The supervisor calls this when a
// workflow's Sleep frame's UntilUnix is past the suspend threshold; the
// row's wake_at drives the scheduler tick that re-spawns the workflow.
func (j *Journal) ScheduleSleep(ctx context.Context, runID, stepName string, wakeAt time.Time) (string, error) {
	return j.ScheduleSleepSeq(ctx, runID, stepName, 0, wakeAt)
}

// ScheduleSleepSeq records the sleep against its per-run call ordinal so a Sleep
// inside a loop gets one row per iteration. seq = 0 keeps legacy semantics.
func (j *Journal) ScheduleSleepSeq(ctx context.Context, runID, stepName string, seq int64, wakeAt time.Time) (string, error) {
	id, err := newID("sched_")
	if err != nil {
		return "", err
	}
	const q = `INSERT INTO schedules (id, run_id, step_name, seq, kind, wake_at, fired)
		VALUES ($1, $2, $3, $4, 'sleep', $5, $6)`
	_, err = j.db.ExecContext(ctx, j.bind(q),
		id, runID, stepName, seq, j.formatTime(wakeAt), j.boolValue(false),
	)
	if err != nil {
		return "", fmt.Errorf("journal: schedule sleep: %w", err)
	}
	return id, nil
}

// ScheduleSignal persists a signal-await schedule. token is the public
// identifier the external HTTP caller posts to; expiresAt is the timeout
// after which the scheduler returns Expired=true to the resumed workflow.
// Both are required so the row participates in both delivery and timeout
// sweeps via a single FindDueSchedules path.
func (j *Journal) ScheduleSignal(ctx context.Context, runID, stepName, signalName, token string, expiresAt time.Time) (string, error) {
	return j.ScheduleSignalSeq(ctx, runID, stepName, 0, signalName, token, expiresAt)
}

// ScheduleSignalSeq records the await against its per-run call ordinal so two
// AwaitSignal calls sharing a signal name occupy two rows instead of one. That
// is what stops the second await from being served the first's already-delivered
// payload. seq = 0 keeps legacy semantics.
func (j *Journal) ScheduleSignalSeq(ctx context.Context, runID, stepName string, seq int64, signalName, token string, expiresAt time.Time) (string, error) {
	id, err := newID("sched_")
	if err != nil {
		return "", err
	}
	const q = `INSERT INTO schedules (id, run_id, step_name, seq, kind, wake_at, signal_name, signal_token, fired)
		VALUES ($1, $2, $3, $4, 'signal', $5, $6, $7, $8)`
	_, err = j.db.ExecContext(ctx, j.bind(q),
		id, runID, stepName, seq, j.formatTime(expiresAt), signalName, token, j.boolValue(false),
	)
	if err != nil {
		return "", fmt.Errorf("journal: schedule signal: %w", err)
	}
	return id, nil
}

// FireSignal records the external delivery for a signal token. The schedule
// stays unfired so the scheduler tick picks it up; the signal_payload
// column is the delivery marker (a NULL payload column means pending,
// a non-NULL payload means delivered awaiting resume). wake_at is bumped
// to now so FindDueSchedules surfaces the row immediately rather than
// waiting for the original timeout.
//
// Returns ErrNotFound when the token doesn't match any signal schedule,
// and ErrAlreadyFired when the schedule already has a payload (idempotent
// retry semantics).
func (j *Journal) FireSignal(ctx context.Context, token string, payload []byte) (runID, signalName string, err error) {
	if len(payload) == 0 {
		// payload-presence is the delivery marker; an empty body would be
		// indistinguishable from "not yet delivered" once the scanner reads
		// it back as nil/empty. Coerce empty bodies to JSON null so every
		// delivered row carries non-zero bytes.
		payload = []byte("null")
	}
	// Prefer the OLDEST row for this token that is still awaiting delivery.
	// The token is derived from the signal NAME, so two AwaitSignal calls
	// sharing a name legitimately share a token; picking an arbitrary row (a
	// bare LIMIT 1) would keep hitting the already-delivered one and report
	// ErrAlreadyFired forever, stranding the pending await. Ordering pending
	// rows first, oldest first, makes repeated deliveries to one signal name
	// fill successive awaits in program order.
	const sel = `SELECT id, run_id, signal_name, signal_payload FROM schedules
		WHERE signal_token = $1 AND kind = 'signal'
		ORDER BY CASE WHEN signal_payload IS NULL THEN 0 ELSE 1 END, created_at ASC
		LIMIT 1`
	row := j.db.QueryRowContext(ctx, j.bind(sel), token)
	var (
		id       string
		runIDOut sql.NullString
		nameOut  sql.NullString
		existing []byte
	)
	if err := row.Scan(&id, &runIDOut, &nameOut, &existing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("journal: lookup signal: %w", err)
	}
	if existing != nil {
		return "", "", ErrAlreadyFired
	}

	const upd = `UPDATE schedules
		SET signal_payload = $1, wake_at = $2
		WHERE id = $3 AND signal_payload IS NULL`
	res, err := j.db.ExecContext(ctx, j.bind(upd),
		payload, j.formatTime(time.Now().UTC()), id,
	)
	if err != nil {
		return "", "", fmt.Errorf("journal: fire signal: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Lost the race to another delivery between SELECT and UPDATE.
		return "", "", ErrAlreadyFired
	}
	return nullableString(runIDOut), nullableString(nameOut), nil
}

// FindDueSchedules returns up to limit unfired schedules whose wake time has
// already elapsed. The scheduler loop walks these and re-spawns each run.
// Implementation note: this is a polling read rather than LISTEN/NOTIFY so
// SQLite + Postgres share one code path; Postgres-only deployments can
// later add a NOTIFY-based fast-path without breaking the contract.
//
// Signal rows are returned alongside sleep rows. A signal row appears here
// either when its timeout has elapsed (signal_payload is NULL, expired
// resume) or because the delivery handler set wake_at = now and left fired
// = false specifically so the next tick processes it.
func (j *Journal) FindDueSchedules(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	rows, err := j.db.QueryContext(ctx, j.bind(j.fairDueScheduleQuery()), j.boolValue(false), j.formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("journal: find due: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		s, err := scanScheduleRow(rows.Scan, j)
		if err != nil {
			// Skip the row instead of failing the whole batch. A single
			// unparseable wake_at (an out-of-range timestamp sorts FIRST as
			// TEXT and always matches "wake_at <= now") would otherwise abort
			// every scheduler tick, so no suspended run in any tenant would
			// ever resume. The supervisor clamps new rows to
			// maxScheduleHorizon; this keeps an already-poisoned table from
			// wedging the daemon.
			j.log.Error("journal: skipping unparseable due schedule row", "err", err)
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FireSchedule marks the schedule fired so the scheduler doesn't re-dispatch
// it during the next tick. Called by the supervisor right before re-spawn.
func (j *Journal) FireSchedule(ctx context.Context, id string) error {
	const q = `UPDATE schedules SET fired = $1 WHERE id = $2`
	res, err := j.db.ExecContext(ctx, j.bind(q), j.boolValue(true), id)
	if err != nil {
		return fmt.Errorf("journal: fire schedule: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("journal: schedule %s not found", id)
	}
	return nil
}

// ClaimSchedule atomically marks an unfired schedule fired and reports
// whether THIS caller won the claim. It is the compare-and-set the
// scheduler uses before re-spawning a suspended run: the UPDATE only
// touches a row that is still fired=false, so two daemons (or a tick
// racing a DLQ retry) can't both resume the same RunID and duplicate its
// side effects. claimed=false means someone else already took it; the
// caller skips silently. A missing row also returns claimed=false.
func (j *Journal) ClaimSchedule(ctx context.Context, id string) (bool, error) {
	const q = `UPDATE schedules SET fired = $1 WHERE id = $2 AND fired = $3`
	res, err := j.db.ExecContext(ctx, j.bind(q), j.boolValue(true), id, j.boolValue(false))
	if err != nil {
		return false, fmt.Errorf("journal: claim schedule: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FindLatestSleepSchedule returns the most recent sleep schedule for a
// (run_id, step_name) pair regardless of fired state. Used by the supervisor
// on re-spawn to decide whether the workflow's repeated Sleep frame is
// already past wake_at (immediate ack) or still pending (re-suspend).
func (j *Journal) FindLatestSleepSchedule(ctx context.Context, runID, stepName string) (Schedule, error) {
	return j.findLatestByKind(ctx, runID, stepName, KindSleep)
}

// FindLatestSignalSchedule returns the most recent signal schedule for a
// (run_id, step_name) pair. The supervisor reads this on resume to decide
// whether to reply SignalDeliver{Payload, Expired:false} (delivered) or
// SignalDeliver{Expired:true} (timeout) without re-suspending.
func (j *Journal) FindLatestSignalSchedule(ctx context.Context, runID, stepName string) (Schedule, error) {
	return j.findLatestByKind(ctx, runID, stepName, KindSignal)
}

// FindScheduleBySeq resolves a schedule by its per-run call ordinal instead of
// by step name, so a Sleep or AwaitSignal repeated in a loop resolves to ITS
// iteration's row. Returns ErrNotFound for seq <= 0 (pre-ordinal binaries use
// the name-keyed lookups) and when the ordinal has not been reached yet.
func (j *Journal) FindScheduleBySeq(ctx context.Context, runID string, seq int64, kind string) (Schedule, error) {
	if seq <= 0 {
		return Schedule{}, ErrNotFound
	}
	const q = `SELECT id, run_id, step_name, kind, wake_at, signal_name, signal_token, signal_payload, fired, created_at
		FROM schedules WHERE run_id = $1 AND seq = $2 AND kind = $3
		ORDER BY created_at DESC LIMIT 1`
	row := j.db.QueryRowContext(ctx, j.bind(q), runID, seq, kind)
	s, err := scanScheduleRow(row.Scan, j)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Schedule{}, ErrNotFound
		}
		return Schedule{}, err
	}
	return s, nil
}

func (j *Journal) findLatestByKind(ctx context.Context, runID, stepName, kind string) (Schedule, error) {
	const q = `SELECT id, run_id, step_name, kind, wake_at, signal_name, signal_token, signal_payload, fired, created_at
		FROM schedules WHERE run_id = $1 AND step_name = $2 AND kind = $3
		ORDER BY created_at DESC LIMIT 1`
	row := j.db.QueryRowContext(ctx, j.bind(q), runID, stepName, kind)
	s, err := scanScheduleRow(row.Scan, j)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Schedule{}, ErrNotFound
		}
		return Schedule{}, err
	}
	return s, nil
}

// scanScheduleRow is the shared scanner for the SELECT shape used by both
// FindDueSchedules (rows) and findLatestByKind (row).
func scanScheduleRow(scan func(...any) error, j *Journal) (Schedule, error) {
	var (
		s          Schedule
		wakeAt     sql.NullString
		signalName sql.NullString
		signalTok  sql.NullString
		payload    []byte
		fired      any
		createdAt  sql.NullString
	)
	if err := scan(&s.ID, &s.RunID, &s.StepName, &s.Kind, &wakeAt, &signalName, &signalTok, &payload, &fired, &createdAt); err != nil {
		return Schedule{}, err
	}
	if wakeAt.Valid {
		t, perr := j.parseTime(wakeAt.String)
		if perr != nil {
			return Schedule{}, fmt.Errorf("journal: parse wake_at: %w", perr)
		}
		s.WakeAt = t
	}
	if signalName.Valid {
		s.SignalName = signalName.String
	}
	if signalTok.Valid {
		s.SignalToken = signalTok.String
	}
	if len(payload) > 0 {
		s.SignalPayload = append([]byte(nil), payload...)
	}
	s.Fired = parseBool(fired)
	if createdAt.Valid {
		t, perr := j.parseTime(createdAt.String)
		if perr == nil {
			s.CreatedAt = t
		}
	}
	return s, nil
}

// SetRunStatus updates runs.status. Used by the supervisor to mark a run
// suspended (sleeping) or restored (running again).
func (j *Journal) SetRunStatus(ctx context.Context, runID, status string) error {
	const q = `UPDATE runs SET status = $1 WHERE id = $2`
	res, err := j.db.ExecContext(ctx, j.bind(q), status, runID)
	if err != nil {
		return fmt.Errorf("journal: set run status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("journal: run %s not found", runID)
	}
	return nil
}

// ResumeSuspendedRun flips a run from 'suspended' back to 'running' as a
// guarded compare-and-set. It returns false (no error) when no row moved: the
// run is no longer suspended because it was cancelled or finalized while it
// slept. The scheduler MUST NOT respawn the workflow in that case - an
// unguarded UPDATE here would overwrite a concurrent cancel and resurrect the
// cancelled run, re-running its side effects.
func (j *Journal) ResumeSuspendedRun(ctx context.Context, runID string) (bool, error) {
	const q = `UPDATE runs SET status = 'running' WHERE id = $1 AND status = 'suspended'`
	res, err := j.db.ExecContext(ctx, j.bind(q), runID)
	if err != nil {
		return false, fmt.Errorf("journal: resume suspended run: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// formatTime renders a time for the engine: ISO-8601 string for SQLite,
// time.Time for Postgres (pgx handles TIMESTAMPTZ natively).
func (j *Journal) formatTime(t time.Time) any {
	if j.engine == EngineSQLite {
		return t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return t.UTC()
}

// parseTime parses what we wrote.
func (j *Journal) parseTime(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("journal: unparseable time %q", s)
}

// boolValue picks the right bool representation per engine. SQLite stores
// 0/1 INTEGER; Postgres has native bool.
func (j *Journal) boolValue(b bool) any {
	if j.engine == EngineSQLite {
		if b {
			return 1
		}
		return 0
	}
	return b
}

// parseBool reads either an int (SQLite) or a bool (Postgres).
func parseBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case []byte:
		return len(x) == 1 && x[0] == 't'
	case string:
		return x == "t" || x == "true" || x == "1"
	}
	return false
}

// newID generates an opaque ID with the given prefix.
func newID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("journal: id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// NewSignalToken generates a 32-byte URL-safe random token. The webhook
// signal endpoint dispatches POST /signal/{token} so the token is the
// per-request capability; brute force is bounded by the 256-bit space.
func NewSignalToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("journal: signal token: %w", err)
	}
	return "sig_" + hex.EncodeToString(b), nil
}

// fairDueScheduleQuery interleaves due schedules ACROSS tenants instead of
// draining them in global wake_at order.
//
// The old query was `WHERE fired = false AND wake_at <= now ORDER BY wake_at ASC
// LIMIT n` with no tenant dimension, so one tenant holding a backlog of due
// sleeps older than everyone else's filled the entire batch every tick and no
// other tenant's suspended run resumed until that backlog drained. That is worse
// than queue starvation: these runs are already mid-execution, so from the other
// tenant's side the workflow has simply hung with no error anywhere.
//
// This mirrors fairCandidateQuery in queue.go, which already solved the same
// problem for run admission, and adds the two tenant gates that resumption was
// bypassing entirely because it never went through CheckWorkflowEnqueueAllowed:
//
//   - disabled tenants are skipped. Disabling a tenant stopped new runs while its
//     suspended ones kept waking up and executing.
//   - max_concurrent_runs is honoured, so a tenant cannot exceed its cap simply by
//     having workflows sleep. The column defaults to 0 (meaning unlimited), so an
//     install that has not deliberately set a cap sees no change; a capped tenant's
//     wake-up is DELAYED rather than dropped, and the ROW_NUMBER interleave keeps
//     that delay from being starvation.
//
// The cap matters in a different way per scheduler mode, which is worth knowing
// before touching this. In DISTRIBUTED mode (Enqueue) the scheduler only flips the
// run to 'queued' and a worker admits it through queue.go's fairCandidateQuery,
// which already applies both gates, so here they merely apply EARLIER and avoid
// claiming a schedule whose run would then sit un-admittable in 'queued'. In
// IN-PROCESS mode the scheduler resumes the run directly and never touches the
// queue, so this is the ONLY place either gate can be applied at all.
//
// The `running` CTE counts status='running' only. A run parked on a sleep is
// 'suspended' (supervisor.go sets that when it suspends), so a sleeping run does
// NOT consume its own tenant's concurrency slot. If it did, a tenant at its cap
// could never wake anything and the cap would be a permanent deadlock rather than
// backpressure.
//
// schedules has no tenant_id of its own; the tenant comes from the run. The join
// is inner on purpose: a schedule whose run row is gone can never dispatch
// successfully, and previously it was returned every tick to fail in the
// scheduler loop.
func (j *Journal) fairDueScheduleQuery() string {
	falseLit := "false"
	if j.engine == EngineSQLite {
		falseLit = "0"
	}
	return `WITH running AS (
		SELECT tenant_id, COUNT(*) AS n FROM runs WHERE status = 'running' GROUP BY tenant_id
	),
	ranked AS (
		SELECT s.id, s.run_id, s.step_name, s.kind, s.wake_at, s.signal_name,
			s.signal_token, s.signal_payload, s.fired, s.created_at, r.tenant_id,
			ROW_NUMBER() OVER (PARTITION BY r.tenant_id ORDER BY s.wake_at, s.id) AS rn
		FROM schedules s
		JOIN runs r ON r.id = s.run_id
		WHERE s.fired = $1 AND s.wake_at IS NOT NULL AND s.wake_at <= $2
	)
	SELECT k.id, k.run_id, k.step_name, k.kind, k.wake_at, k.signal_name,
		k.signal_token, k.signal_payload, k.fired, k.created_at
	FROM ranked k
	LEFT JOIN tenants t ON t.tenant_id = k.tenant_id
	LEFT JOIN running ru ON ru.tenant_id = k.tenant_id
	WHERE (t.disabled IS NULL OR t.disabled = ` + falseLit + `)
		AND (t.max_concurrent_runs IS NULL OR t.max_concurrent_runs <= 0
			OR k.rn <= t.max_concurrent_runs - COALESCE(ru.n, 0))
	ORDER BY k.rn, k.wake_at, k.id
	LIMIT $3`
}
