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
	id, err := newID("sched_")
	if err != nil {
		return "", err
	}
	const q = `INSERT INTO schedules (id, run_id, step_name, kind, wake_at, fired)
		VALUES ($1, $2, $3, 'sleep', $4, $5)`
	_, err = j.db.ExecContext(ctx, j.bind(q),
		id, runID, stepName, j.formatTime(wakeAt), j.boolValue(false),
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
	id, err := newID("sched_")
	if err != nil {
		return "", err
	}
	const q = `INSERT INTO schedules (id, run_id, step_name, kind, wake_at, signal_name, signal_token, fired)
		VALUES ($1, $2, $3, 'signal', $4, $5, $6, $7)`
	_, err = j.db.ExecContext(ctx, j.bind(q),
		id, runID, stepName, j.formatTime(expiresAt), signalName, token, j.boolValue(false),
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
	const sel = `SELECT id, run_id, signal_name, signal_payload FROM schedules
		WHERE signal_token = $1 AND kind = 'signal' LIMIT 1`
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
	const q = `SELECT id, run_id, step_name, kind, wake_at, signal_name, signal_token, signal_payload, fired, created_at
		FROM schedules WHERE fired = $1 AND wake_at IS NOT NULL AND wake_at <= $2
		ORDER BY wake_at ASC LIMIT $3`
	rows, err := j.db.QueryContext(ctx, j.bind(q), j.boolValue(false), j.formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("journal: find due: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		s, err := scanScheduleRow(rows.Scan, j)
		if err != nil {
			return nil, err
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
