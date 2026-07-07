// Package supervisor runs a single workflow subprocess, dispatches its
// wire-protocol frames, and persists step outcomes into the journal so
// the workflow can survive mid-step host restarts.
//
// One Supervisor per run. The supervisor spawns the workflow binary,
// wires its stdin + stdout, sends the Hello frame, and then loops:
//
//   wf -> step_start  -> host checks journal -> reply proceed | replay
//   wf -> step_end    -> host writes journal -> reply ack
//   wf -> sleep       -> host blocks (short) or suspends (long, week 4) -> ack
//   wf -> secret_fetch -> host reads vault   -> reply with bytes
//   wf -> log         -> host forwards to slog (no reply)
//
// On EOF (workflow exits), the supervisor marks the run terminal
// (succeeded if last step succeeded, failed otherwise).
package supervisor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/brightinteraction/reactor/internal/runtime/journal"
	"github.com/brightinteraction/reactor/internal/runtime/wire"
	"github.com/brightinteraction/reactor/internal/vault"
)

// VaultReader is the metadata + value surface the supervisor needs from the
// vault. Defined locally so the supervisor doesn't drag in the rotation
// scheduler or rotator interfaces.
type VaultReader interface {
	Get(ctx context.Context, id string) (*vault.Secret, error)
}

// OAuthTokenResolver returns a fresh access token for a connection id scoped to
// a tenant. Satisfied by *oauth.Store; an interface here so the supervisor
// package doesn't import internal/oauth.
type OAuthTokenResolver interface {
	Token(ctx context.Context, connectionID, tenantID string) (string, error)
}

// Supervisor wires a journal + vault + a workflow binary into a running
// workflow.
type Supervisor struct {
	BinaryPath   string
	WorkflowSlug string
	RunID        string
	Mode         string // "live" | "replay" | "dry_run"
	Journal      *journal.Journal
	Vault        VaultReader
	// OAuthTokens resolves an `oauth:<connection-id>` secret request to a
	// fresh access token (refreshing if needed), scoped to the run's tenant.
	// nil disables oauth: resolution.
	OAuthTokens  OAuthTokenResolver
	Log          *slog.Logger

	// SignalSigningKey is the root secret (the vault master key) used to derive
	// a per-run signing key for AwaitSignal capability tokens. Set from the
	// master key at wiring time. Empty falls back to the legacy runID-only
	// derivation (used by tests without a vault); production always sets it so
	// the token is unforgeable by anyone who only knows the semi-public runID.
	SignalSigningKey []byte

	// Input is the JSON-encoded trigger payload. Surfaced to the
	// workflow via the REACTOR_INPUT env var so sdk/runtime.Serve's
	// generic Input I decode finds it. Empty means the workflow
	// receives the zero value.
	Input []byte

	// ExtraEnv is appended to os.Environ() when spawning the child.
	// Tests inject test-only env vars (FF_TEST_RECORD, etc.) here so
	// they survive across spawns without polluting the parent process.
	ExtraEnv []string

	// SuspendThreshold is the cutoff for short-vs-long sleeps. A Sleep
	// frame with wake_at <= now + threshold blocks the supervisor in
	// place; anything longer is upgraded to suspend-and-resume: a
	// schedules row is written, the workflow process is asked to cancel,
	// and the scheduler re-spawns it at wake_at. Default 30s.
	SuspendThreshold time.Duration

	// Optional clock override for tests. Ignored by the kernel sleep
	// itself; only used to evaluate UntilUnix vs threshold and for
	// schedule timestamps.
	Now func() time.Time

	// LogSink optionally receives workflow-emitted log lines (Log
	// frames from the workflow process + the workflow's stderr). The
	// daemon wires this to internal/runlogs.Buffer.Append so the
	// dashboard's /runs/{id}/tail SSE shows the workflow's own logs
	// alongside dispatcher status lines. Nil means logs only reach
	// the slog sink.
	LogSink func(runID, line string)

	// Limits caps the workflow subprocess via prlimit(2) on Linux.
	// Zero fields fall back to DefaultResourceLimits. Non-Linux GOOS
	// values are no-op (operators rely on the outer container/VM).
	Limits ResourceLimits

	// ACLPermissive controls how workflow_secret_grants emptiness is
	// interpreted. false (default): empty table denies every fetch,
	// forcing operators to explicitly grant credentials per workflow.
	// true: empty table allows every fetch (the legacy v0 behaviour;
	// kept for migration scenarios via REACTOR_VAULT_ACL_PERMISSIVE=1).
	ACLPermissive bool
}

// Run spawns the workflow binary and dispatches its frames until EOF or
// ctx cancellation. Returns the run's terminal status.
func (s *Supervisor) Run(ctx context.Context) (string, error) {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.SuspendThreshold == 0 {
		s.SuspendThreshold = 30 * time.Second
	}

	cmd := exec.CommandContext(ctx, s.BinaryPath)
	// The child workflow runs untrusted, operator-or-AI-authored code. It
	// must NOT inherit the daemon's full environment: os.Environ() would
	// hand it REACTOR_MASTER_KEY + REACTOR_DB_URL, letting any workflow
	// decrypt the entire vault and bypass the per-workflow secret ACL.
	// Secrets reach the workflow only over the wire protocol's
	// secret_fetch frame (gated by HasGrant). So we build the child env
	// from an explicit allowlist of harmless system vars plus the one
	// input var and any test-injected ExtraEnv.
	env := childEnv(s.Input, s.ExtraEnv)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "failed", fmt.Errorf("supervisor: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "failed", fmt.Errorf("supervisor: stdout: %w", err)
	}
	cmd.Stderr = newStderrForwarder(s.Log)

	// Optional cgroup v2 layer. Linux + /sys/fs/cgroup mounted as v2 +
	// daemon has write perms = the child clone3()s straight into a
	// per-run cgroup with memory.max + pids.max preset; the noop path
	// returns Fd=-1 and we still get prlimit enforcement below.
	cgroup := prepareCgroup(s.Log, s.RunID, s.Limits)
	defer closeCgroupHandle(cgroup)
	if cgroup.Fd >= 0 {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		applyCgroupToSysProcAttr(cmd.SysProcAttr, cgroup)
	}

	if err := cmd.Start(); err != nil {
		return "failed", fmt.Errorf("supervisor: start: %w", err)
	}

	if err := applyRlimits(cmd.Process.Pid, s.Limits); err != nil {
		s.Log.Warn("supervisor: rlimit apply failed", "err", err)
	}

	disp := &dispatcher{
		sup:     s,
		enc:     wire.NewEncoder(stdin),
		dec:     wire.NewDecoder(stdout),
		writeMu: &sync.Mutex{},
	}

	if err := disp.sendHello(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "failed", fmt.Errorf("supervisor: hello: %w", err)
	}

	loopErr := disp.loop(ctx)
	if errors.Is(loopErr, errSuspended) {
		loopErr = nil // not an error; it's a clean suspend
	}

	closeErr := stdin.Close()
	waitErr := cmd.Wait()

	terminal := "succeeded"
	if loopErr != nil && !errors.Is(loopErr, io.EOF) {
		terminal = "failed"
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && !exitErr.Success() {
			terminal = "failed"
		}
	}
	// If the dispatcher upgraded a long Sleep to a suspend, the run is
	// not terminal; it stays in "suspended" state and the scheduler will
	// re-spawn at wake_at. Don't overwrite that with succeeded/failed.
	if disp.suspended.Load() {
		if err := s.Journal.SetRunStatus(ctx, s.RunID, "suspended"); err != nil {
			s.Log.Warn("set suspended status failed", "err", err)
		}
		return "suspended", nil
	}
	if disp.deadLetter.Load() {
		terminal = "failed_dlq"
	}
	if mErr := s.Journal.MarkRunFinished(ctx, s.RunID, terminal); mErr != nil {
		s.Log.Warn("mark run finished failed", "err", mErr)
	}
	if closeErr != nil && !errors.Is(closeErr, io.ErrClosedPipe) {
		s.Log.Warn("supervisor: stdin close", "err", closeErr)
	}
	if loopErr != nil && !errors.Is(loopErr, io.EOF) {
		return terminal, loopErr
	}
	return terminal, nil
}

// dispatcher owns one live workflow connection.
type dispatcher struct {
	sup       *Supervisor
	enc       *wire.Encoder
	dec       *wire.Decoder
	writeMu   *sync.Mutex
	frames    atomic.Int64
	suspended atomic.Bool // set when a long Sleep upgrades to suspend
	deadLetter atomic.Bool // set when a step's final attempt failed and was DLQ'd
}

func (d *dispatcher) sendHello() error {
	f, err := wire.Wrap(d.nextID(), 0, wire.KindHello, wire.Hello{
		Version:      wire.Version,
		WorkflowSlug: d.sup.WorkflowSlug,
		RunID:        d.sup.RunID,
		Mode:         d.sup.Mode,
		SignalKey:    perRunSignalKey(d.sup.SignalSigningKey, d.sup.RunID),
	})
	if err != nil {
		return err
	}
	return d.write(f)
}

func (d *dispatcher) loop(ctx context.Context) error {
	for {
		f, err := d.dec.Decode()
		if err != nil {
			return err
		}
		switch f.Kind {
		case wire.KindHello:
			// First frame from workflow: confirms protocol match. Nothing else.
		case wire.KindStepStart:
			if err := d.handleStepStart(ctx, f); err != nil {
				return err
			}
		case wire.KindStepEnd:
			if err := d.handleStepEnd(ctx, f); err != nil {
				return err
			}
		case wire.KindSleep:
			if err := d.handleSleep(ctx, f); err != nil {
				return err
			}
		case wire.KindAwaitSignal:
			if err := d.handleAwaitSignal(ctx, f); err != nil {
				return err
			}
		case wire.KindSecretFetch:
			if err := d.handleSecretFetch(ctx, f); err != nil {
				return err
			}
		case wire.KindLog:
			d.handleLog(f)
		default:
			d.sup.Log.Warn("supervisor: unknown frame kind", "kind", f.Kind)
		}
	}
}

func (d *dispatcher) handleStepStart(ctx context.Context, f wire.Frame) error {
	var body wire.StepStart
	if err := wire.Unwrap(f, &body); err != nil {
		return err
	}

	cached, err := d.sup.Journal.FindCachedOutputForInput(ctx, d.sup.RunID, body.StepName, body.IdempotencyKey, body.InputHash)
	if err == nil {
		// Already succeeded against this exact input; tell the workflow
		// to use the cache. Input drift (workflow author changed the
		// Step input shape) falls through to the not-found branch and
		// triggers re-execution in live mode or divergence in replay.
		reply, _ := wire.Wrap(d.nextID(), f.ID, wire.KindStepReply, wire.StepReply{Replay: true, Output: cached})
		return d.write(reply)
	}
	if !errors.Is(err, journal.ErrNotFound) {
		return fmt.Errorf("supervisor: journal lookup: %w", err)
	}

	// Replay mode forbids running new step closures. The operator invoked
	// `reactor replay <run-id>` against a historical run; if a step has
	// no cached output (for this exact input_hash) it means the workflow
	// source has diverged from the recorded DAG, so we abort cleanly
	// rather than re-execute side effects. Distinguish input-drift
	// (a cache row exists for this step under a different input) from
	// fully-missing-step so the diagnostic is actionable.
	if d.sup.Mode == "replay" {
		anyExists, probeErr := d.sup.Journal.HasCachedOutputAnyInput(ctx, d.sup.RunID, body.StepName)
		if probeErr == nil && anyExists {
			return fmt.Errorf("%w: step %q input drifted (input_hash mismatch with recorded run)", ErrReplayDivergence, body.StepName)
		}
		return fmt.Errorf("%w: step %q has no cached output", ErrReplayDivergence, body.StepName)
	}

	// Insert a running attempt row. Best-effort: if the row already exists
	// (concurrent supervisor for the same run, which should never happen
	// thanks to leases), the journal returns inserted=false and we still
	// reply proceed; the second step_end will overwrite the first.
	if _, err := d.sup.Journal.RecordStepStart(ctx, d.sup.RunID, body.StepName, body.Attempt, body.IdempotencyKey, body.InputHash); err != nil {
		return fmt.Errorf("supervisor: record step_start: %w", err)
	}
	reply, _ := wire.Wrap(d.nextID(), f.ID, wire.KindStepReply, wire.StepReply{Replay: false})
	return d.write(reply)
}

// ErrReplayDivergence signals that a replay run hit a step with no cached
// output. The operator's workflow source has changed since the recorded
// run; replay can't proceed without re-executing side effects.
var ErrReplayDivergence = errors.New("supervisor: replay divergence")

func (d *dispatcher) handleStepEnd(ctx context.Context, f wire.Frame) error {
	var body wire.StepEnd
	if err := wire.Unwrap(f, &body); err != nil {
		return err
	}
	out := body.Output
	if len(out) == 0 {
		out = json.RawMessage("null")
	}
	if err := d.sup.Journal.RecordStepEnd(ctx, d.sup.RunID, body.StepName, body.Attempt, out, body.ErrorText); err != nil {
		return fmt.Errorf("supervisor: record step_end: %w", err)
	}
	// Final-attempt failures land in the dead-letter queue so an operator
	// can inspect or replay them without grovelling through the steps
	// table. Retryable errors are kept out of DLQ; the workflow side will
	// emit another step_start with a higher attempt number for them.
	// (Today the SDK is single-attempt; the Retryable flag still gates
	// DLQ for forward-compatibility with host-side retries.)
	if body.ErrorText != "" && !body.Retryable {
		payload := out
		if len(payload) == 0 || string(payload) == "null" {
			payload = json.RawMessage("{}")
		}
		if err := d.sup.Journal.MoveStepToDeadLetter(ctx, d.sup.RunID, body.StepName, body.ErrorText, payload); err != nil {
			d.sup.Log.Warn("supervisor: dead-letter write failed", "err", err, "run_id", d.sup.RunID, "step", body.StepName)
		} else {
			d.deadLetter.Store(true)
		}
	}
	ack, _ := wire.Wrap(d.nextID(), f.ID, wire.KindAck, nil)
	return d.write(ack)
}

func (d *dispatcher) handleSleep(ctx context.Context, f wire.Frame) error {
	var body wire.Sleep
	if err := wire.Unwrap(f, &body); err != nil {
		return err
	}

	// Resume path: if a previous suspend wrote a sleep schedule for this
	// (run_id, step_name), pin to its wake_at instead of the freshly-
	// recomputed UntilUnix. The SDK can't help here because time.Now() in
	// the workflow body advances across the suspend/respawn boundary, so
	// the second-spawn UntilUnix is always now+d which would re-suspend
	// forever. The journal row is the source of truth for wake_at.
	existing, err := d.sup.Journal.FindLatestSleepSchedule(ctx, d.sup.RunID, body.StepName)
	switch {
	case err == nil:
		if !existing.WakeAt.After(d.sup.Now()) {
			ack, _ := wire.Wrap(d.nextID(), f.ID, wire.KindAck, nil)
			return d.write(ack)
		}
		// Schedule exists but wake_at is still in the future (rare: a
		// replay happened before wake). Re-suspend on the existing
		// wake_at so the scheduler tick handles the rest.
		return d.suspendForSleep(ctx, body.StepName, existing.WakeAt, false)
	case errors.Is(err, journal.ErrNotFound):
		// Fresh path; fall through.
	default:
		return fmt.Errorf("supervisor: lookup sleep schedule: %w", err)
	}

	until := time.Unix(body.UntilUnix, 0)
	wait := until.Sub(d.sup.Now())

	// Already past wake on the first call: the workflow body computed an
	// UntilUnix in the past (e.g. negative duration). Ack immediately.
	if wait <= 0 {
		ack, _ := wire.Wrap(d.nextID(), f.ID, wire.KindAck, nil)
		return d.write(ack)
	}

	// Short sleep: block in place. The supervisor's lifetime is bounded
	// by SuspendThreshold so this can't pin a goroutine for days.
	if wait <= d.sup.SuspendThreshold {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		ack, _ := wire.Wrap(d.nextID(), f.ID, wire.KindAck, nil)
		return d.write(ack)
	}

	// Long sleep: write a schedules row pinned to the first-call until,
	// then suspend. Subsequent respawns enter the resume branch above.
	return d.suspendForSleep(ctx, body.StepName, until, true)
}

// suspendForSleep is the shared tail of the fresh + resume long-sleep
// paths. It writes (or relies on an existing) schedules row, marks the
// run suspended, sends Cancel so the workflow process exits cleanly,
// and returns errSuspended so the dispatcher loop unwinds.
func (d *dispatcher) suspendForSleep(ctx context.Context, stepName string, wakeAt time.Time, writeRow bool) error {
	if writeRow {
		if _, err := d.sup.Journal.ScheduleSleep(ctx, d.sup.RunID, stepName, wakeAt); err != nil {
			return fmt.Errorf("supervisor: schedule sleep: %w", err)
		}
	}
	d.suspended.Store(true)
	cancel, _ := wire.Wrap(d.nextID(), 0, wire.KindCancel, wire.Cancel{
		Reason: fmt.Sprintf("suspend until %s for step %q", wakeAt.Format(time.RFC3339), stepName),
	})
	if err := d.write(cancel); err != nil {
		return err
	}
	// Returning io.EOF here would conflate with pipe closure; use a
	// sentinel that the loop translates to a clean exit.
	return errSuspended
}

// errSuspended is the sentinel that handleSleep returns when it has
// upgraded a long Sleep to a suspend. The dispatcher loop unwinds, the
// supervisor closes stdin, the workflow process notices Cancel + EOF and
// exits 0.
var errSuspended = errors.New("supervisor: workflow suspended on long sleep")

// handleAwaitSignal persists or resumes a signal-await schedule.
//
// Fresh path: workflow's first call. The supervisor derives a deterministic
// token from (run_id, signal_name), writes a schedules row keyed by that
// token, asks the workflow process to exit, and lets the scheduler tick
// re-spawn the run when either the external HTTP delivery or the timeout
// expiry fires.
//
// Resume path: workflow re-runs Run() after a scheduler resume. The
// supervisor finds the existing schedule for (run_id, step_name); if a
// payload was delivered, it replies SignalDeliver{Payload} so the SDK
// returns the value. If wake_at has elapsed without a payload, it replies
// SignalDeliver{Expired:true} so AwaitSignal returns context.DeadlineExceeded.
func (d *dispatcher) handleAwaitSignal(ctx context.Context, f wire.Frame) error {
	var body wire.AwaitSignal
	if err := wire.Unwrap(f, &body); err != nil {
		return err
	}

	existing, err := d.sup.Journal.FindLatestSignalSchedule(ctx, d.sup.RunID, body.StepName)
	switch {
	case err == nil:
		// Resume path. Decide based on payload presence and wake_at.
		if len(existing.SignalPayload) > 0 {
			reply, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSignalDeliver, wire.SignalDeliver{
				SignalName: existing.SignalName,
				Token:      existing.SignalToken,
				Payload:    json.RawMessage(existing.SignalPayload),
			})
			return d.write(reply)
		}
		if !existing.WakeAt.IsZero() && !existing.WakeAt.After(d.sup.Now()) {
			reply, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSignalDeliver, wire.SignalDeliver{
				SignalName: existing.SignalName,
				Token:      existing.SignalToken,
				Expired:    true,
			})
			return d.write(reply)
		}
		// Schedule exists but neither delivered nor expired (race: replay
		// happened between schedule creation and timeout). Re-suspend on
		// the same token so external deliveries against it still resolve.
		return d.suspendForSignal(body, existing.SignalToken)
	case errors.Is(err, journal.ErrNotFound):
		// Fresh path.
	default:
		return fmt.Errorf("supervisor: lookup signal schedule: %w", err)
	}

	timeout := time.Duration(body.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 365 * 24 * time.Hour
	}
	expiresAt := d.sup.Now().Add(timeout)
	token := DeriveSignalToken(perRunSignalKey(d.sup.SignalSigningKey, d.sup.RunID), d.sup.RunID, body.SignalName)
	if _, err := d.sup.Journal.ScheduleSignal(ctx, d.sup.RunID, body.StepName, body.SignalName, token, expiresAt); err != nil {
		return fmt.Errorf("supervisor: schedule signal: %w", err)
	}
	// Log only the token fingerprint (first 8 chars). The full token is a
	// capability that grants the holder POST /signal/{token} access
	// without HMAC; anyone reading the log otherwise can resume any
	// suspended workflow with attacker-controlled payload.
	d.sup.Log.Info("signal registered",
		"run_id", d.sup.RunID, "step", body.StepName, "signal", body.SignalName,
		"token_fp", tokenFingerprint(token), "expires_at", expiresAt.UTC().Format(time.RFC3339))
	return d.suspendForSignal(body, token)
}

// suspendForSignal sends Cancel to the workflow process, marks the run
// suspended, and returns errSuspended so the dispatcher loop unwinds.
func (d *dispatcher) suspendForSignal(body wire.AwaitSignal, token string) error {
	d.suspended.Store(true)
	cancel, _ := wire.Wrap(d.nextID(), 0, wire.KindCancel, wire.Cancel{
		Reason: fmt.Sprintf("awaiting signal %q (token=%s)", body.SignalName, token),
	})
	if err := d.write(cancel); err != nil {
		return err
	}
	return errSuspended
}

// tokenFingerprint returns a short, non-reversible identifier for a
// capability token so it can be logged for correlation without leaking
// the token itself. 8 hex chars is enough to ground a log search; the
// full token stays in the schedules table only.
func tokenFingerprint(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:8]
}

// signalKeyPurpose domain-separates the per-run signal signing key derivation
// from any other use of the master key.
const signalKeyPurpose = "reactor-signal-v1:"

// perRunSignalKey derives the per-run signing key (hex) from the root signing
// key (the vault master key) and the runID: HMAC(master, "reactor-signal-v1:"+
// runID). It is stable across suspend/resume (master + runID are stable) and is
// delivered to the workflow over the private Hello frame so the SDK computes
// the same token. Returns "" when no signing key is wired (legacy/test paths),
// which selects the legacy runID-only derivation on both sides.
func perRunSignalKey(signingKey []byte, runID string) string {
	if len(signingKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte(signalKeyPurpose + runID))
	return hex.EncodeToString(mac.Sum(nil))
}

// DeriveSignalToken produces the capability token for a signal-await. Both the
// supervisor and the SDK compute the same value so workflow code can embed the
// public delivery URL (POST /signal/{token}) in user-facing content (approval
// emails, Slack DMs) before the run suspends.
//
// When perRunKeyHex is set, the token is HMAC(perRunKey, signalName): the
// per-run key is a secret derived from the master key and shared with the
// workflow only over the private Hello frame, so a party who knows only the
// (semi-public, log/URL-exposed) runID CANNOT forge the token. When it is empty
// (no signing key wired), it falls back to the legacy runID-only SHA-256; this
// keeps signal-less test paths working but is NOT unforgeable, so production
// always wires SignalSigningKey. signalName namespaces multiple awaits per run.
func DeriveSignalToken(perRunKeyHex, runID, signalName string) string {
	if perRunKeyHex != "" {
		key, err := hex.DecodeString(perRunKeyHex)
		if err == nil && len(key) > 0 {
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(signalName))
			return "sig_" + hex.EncodeToString(mac.Sum(nil)[:16])
		}
	}
	h := sha256.Sum256([]byte(runID + ":signal:" + signalName))
	return "sig_" + hex.EncodeToString(h[:16])
}

// oauthTenant resolves the run's tenant (to scope an oauth: connection lookup).
// Returns "" when it can't be established, which the caller treats as deny.
func (d *dispatcher) oauthTenant(ctx context.Context) string {
	if d.sup.WorkflowSlug == "" || d.sup.Journal == nil {
		return ""
	}
	wfID, err := d.sup.Journal.WorkflowIDBySlug(ctx, d.sup.WorkflowSlug)
	if err != nil {
		return ""
	}
	tenant, err := d.sup.Journal.WorkflowTenant(ctx, wfID)
	if err != nil {
		return ""
	}
	return tenant
}

func (d *dispatcher) handleSecretFetch(ctx context.Context, f wire.Frame) error {
	var body wire.SecretFetch
	if err := wire.Unwrap(f, &body); err != nil {
		return err
	}

	// Per-workflow secret ACL gate. Resolve workflow_id once + check
	// against workflow_secret_grants. ErrACLEmpty means the table has
	// no rows; we fall back to permissive mode so fresh installs and
	// upgrades-from-pre-0007 don't break. Any other error from the
	// grant probe is logged + treated as deny so a misbehaving DB
	// doesn't accidentally hand out credentials.
	//
	// An empty WorkflowSlug or a slug we cannot resolve into a workflow_id
	// is treated as deny (under strict mode) rather than bypass: a missing
	// identity means we cannot make a grant decision, and strict-deny is
	// the safer default. Permissive mode still allows the fetch so the
	// migration story is unchanged.
	if !d.sup.ACLPermissive {
		if d.sup.WorkflowSlug == "" {
			d.sup.Log.Warn("supervisor: secret access denied (no workflow slug on this supervisor; cannot make a grant decision)",
				"credential_id", body.ID)
			deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
			return d.write(deny)
		}
		wfID, err := d.sup.Journal.WorkflowIDBySlug(ctx, d.sup.WorkflowSlug)
		if err != nil {
			d.sup.Log.Warn("supervisor: secret access denied (workflow lookup failed)",
				"err", err, "workflow", d.sup.WorkflowSlug, "credential_id", body.ID)
			deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
			return d.write(deny)
		}
		ok, err := d.sup.Journal.HasGrant(ctx, wfID, body.ID)
		switch {
		case errors.Is(err, journal.ErrACLEmpty):
			// Fail CLOSED on an empty table. Do NOT steer the operator toward
			// REACTOR_VAULT_ACL_PERMISSIVE=1 (that opens the vault to every
			// workflow); the secure fix is to seed the one grant this workflow
			// needs.
			d.sup.Log.Warn("supervisor: secret access denied (no secret grants configured; grant this workflow the credential with `reactor vault grant <workflow> <credential>`)",
				"workflow", d.sup.WorkflowSlug, "credential_id", body.ID)
			deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
			return d.write(deny)
		case err != nil:
			d.sup.Log.Warn("supervisor: grant check failed; denying",
				"err", err, "workflow", d.sup.WorkflowSlug, "credential_id", body.ID)
			deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
			return d.write(deny)
		case !ok:
			d.sup.Log.Warn("supervisor: secret access denied (no grant)",
				"workflow", d.sup.WorkflowSlug, "credential_id", body.ID)
			deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
			return d.write(deny)
		}
	} else if d.sup.WorkflowSlug != "" {
		// Permissive mode still emits an audit gate when slug is set:
		// the path mirrors the strict branch but skips the deny on
		// ACLEmpty so legacy zero-grant installs continue to function.
		wfID, err := d.sup.Journal.WorkflowIDBySlug(ctx, d.sup.WorkflowSlug)
		if err == nil {
			ok, gErr := d.sup.Journal.HasGrant(ctx, wfID, body.ID)
			switch {
			case errors.Is(gErr, journal.ErrACLEmpty):
				// Permissive + empty table = every workflow can read every
				// credential. This is the documented v0 escape hatch, but it is
				// NOT silent: warn loudly on each allow so a permissive install
				// with no grants is visible in the logs, not a quiet open door.
				d.sup.Log.Warn("supervisor: SECRET ACL WIDE OPEN (REACTOR_VAULT_ACL_PERMISSIVE=1 and no grants seeded); this workflow was allowed a credential with no grant. Seed grants and unset the permissive flag.",
					"workflow", d.sup.WorkflowSlug, "credential_id", body.ID)
			case gErr == nil && !ok:
				d.sup.Log.Warn("supervisor: secret access denied (no grant) [permissive]",
					"workflow", d.sup.WorkflowSlug, "credential_id", body.ID)
				deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
				return d.write(deny)
			}
		}
	}

	// OAuth connection tokens: `oauth:<connection-id>` resolves to a fresh
	// access token (auto-refreshed), scoped to this run's tenant so a workflow
	// can only use its own tenant's connections. The ACL grant above gates it
	// like any other credential id.
	if rest, isOAuth := strings.CutPrefix(body.ID, "oauth:"); isOAuth {
		if d.sup.OAuthTokens == nil {
			deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
			return d.write(deny)
		}
		tenantID := d.oauthTenant(ctx)
		if tenantID == "" {
			// Can't establish the run's tenant -> can't safely scope the
			// connection lookup; deny rather than resolve unscoped.
			d.sup.Log.Warn("supervisor: oauth denied (could not resolve run tenant)", "connection", rest)
			deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
			return d.write(deny)
		}
		tok, terr := d.sup.OAuthTokens.Token(ctx, rest, tenantID)
		if terr != nil || tok == "" {
			d.sup.Log.Warn("supervisor: oauth token resolution failed", "connection", rest, "err", terr)
			deny, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
			return d.write(deny)
		}
		reply, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{Value: []byte(tok)})
		return d.write(reply)
	}

	sec, err := d.sup.Vault.Get(ctx, body.ID)
	if err != nil {
		reply, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{NotFound: true})
		return d.write(reply)
	}
	reply, _ := wire.Wrap(d.nextID(), f.ID, wire.KindSecretReply, wire.SecretReply{
		Value:       sec.Reveal(),
		Fingerprint: sec.Fingerprint(),
	})
	return d.write(reply)
}

func (d *dispatcher) handleLog(f wire.Frame) {
	var body wire.Log
	if err := wire.Unwrap(f, &body); err != nil {
		return
	}
	level := slog.LevelInfo
	switch body.Level {
	case "DEBUG", "debug":
		level = slog.LevelDebug
	case "WARN", "warn":
		level = slog.LevelWarn
	case "ERROR", "error":
		level = slog.LevelError
	}
	attrs := make([]any, 0, 2+len(body.Attrs)*2)
	attrs = append(attrs, "run_id", d.sup.RunID)
	for k, v := range body.Attrs {
		attrs = append(attrs, k, v)
	}
	d.sup.Log.Log(context.Background(), level, body.Msg, attrs...)
	if d.sup.LogSink != nil {
		// Render a flat single-line shape for the SSE tail. slog's text
		// handler shape is overkill for the dashboard; this stays
		// human-readable while round-tripping through SSE's data: lines.
		var rendered = body.Msg
		if len(body.Attrs) > 0 {
			rendered += renderAttrs(body.Attrs)
		}
		d.sup.LogSink(d.sup.RunID, "workflow: "+body.Level+" "+rendered)
	}
}

// renderAttrs flattens a workflow-emitted attrs map into a key=value
// suffix string. Order is map-iteration (non-deterministic) but for
// log-tail UX the legibility cost is acceptable + the lines are still
// the same in the slog handler which uses its own ordering.
func renderAttrs(attrs map[string]any) string {
	if len(attrs) == 0 {
		return ""
	}
	out := " ["
	first := true
	for k, v := range attrs {
		if !first {
			out += " "
		}
		first = false
		out += fmt.Sprintf("%s=%v", k, v)
	}
	out += "]"
	return out
}

// childEnvAllowlist is the set of environment variables a workflow
// subprocess is allowed to inherit from the daemon. Everything else (the
// vault master key, the DB URL, ANTHROPIC_API_KEY, any other REACTOR_*
// secret) is withheld; secrets reach the workflow only through the
// gated secret_fetch wire frame. SSL_CERT_* stay in so workflows making
// outbound HTTPS calls find the system CA bundle inside slim containers.
var childEnvAllowlist = map[string]struct{}{
	"PATH": {}, "HOME": {}, "TMPDIR": {}, "TMP": {}, "TEMP": {},
	"TZ": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "LC_NUMERIC": {},
	"USER": {}, "LOGNAME": {}, "TERM": {}, "PWD": {},
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
}

// childEnv builds the subprocess environment from the allowlist plus the
// trigger input and any test-injected ExtraEnv. Returning a fresh slice
// (never os.Environ()) is the security boundary that keeps the daemon's
// secrets out of untrusted workflow code.
func childEnv(input []byte, extra []string) []string {
	env := make([]string, 0, len(childEnvAllowlist)+len(extra)+1)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if _, ok := childEnvAllowlist[kv[:eq]]; ok {
			env = append(env, kv)
		}
	}
	if len(input) > 0 {
		env = append(env, "REACTOR_INPUT="+string(input))
	}
	env = append(env, extra...)
	return env
}

func (d *dispatcher) nextID() int64 { return d.frames.Add(1) }

func (d *dispatcher) write(f wire.Frame) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.enc.Encode(f)
}

// stderrForwarder echoes stderr lines as warn-level slog events.
type stderrForwarder struct{ log *slog.Logger }

func newStderrForwarder(log *slog.Logger) io.Writer { return stderrForwarder{log: log} }

func (s stderrForwarder) Write(p []byte) (int, error) {
	s.log.Warn("workflow stderr", "line", string(p))
	return len(p), nil
}
