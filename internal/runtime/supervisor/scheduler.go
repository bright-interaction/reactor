package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brightinteraction/reactor/internal/runtime/cancelreg"
	"github.com/brightinteraction/reactor/internal/runtime/journal"
)

// Scheduler walks the schedules table and re-spawns suspended runs whose
// wake time has elapsed. One Scheduler per Reactor process; it owns no
// state beyond its config and the journal handle.
//
// Tick semantics:
//
//  1. Query FindDueSchedules(now, batch).
//  2. For each row: mark fired (so the next tick skips it), spawn a new
//     Supervisor with the same RunID, and let it Run() to completion or
//     re-suspend.
//  3. The new supervisor will replay journal entries for prior steps;
//     when the workflow's Sleep frame arrives, the host sees wait <= 0
//     and immediately acks.
//
// In v0 the scheduler runs each due row sequentially. A worker pool +
// per-run lease handover lands when external workers come online (week 7+).
type Scheduler struct {
	Journal *journal.Journal
	Vault   VaultReader
	Log     *slog.Logger

	// BinaryPath is the workflow binary the supervisor exec's. In v0
	// every workflow shares one binary path; multi-workflow indexing
	// (per-slug binary lookup) lands week 6 with the codegen pipeline.
	BinaryPath func(workflowSlug string) (string, error)

	// Now overrides the clock for tests. Defaults to time.Now.
	Now func() time.Time

	// Tick interval for the polling loop. Defaults to 5 seconds; tests
	// can drop to milliseconds for fast iteration.
	TickInterval time.Duration

	// Batch caps schedules processed per tick. Defaults to 50.
	Batch int

	// SupervisorTemplate is copied as the base config for spawned
	// Supervisors. RunID, BinaryPath, WorkflowSlug, Mode are set per row.
	SupervisorTemplate Supervisor

	// OnTerminal fires after a resumed run reaches a real terminal status
	// (anything but "suspended"). The daemon wires this to the same
	// notifier + chain-firing logic the dispatcher's OnTerminal runs, so
	// a workflow that slept or awaited a signal still sends its failure /
	// success alerts and fires downstream chains when it finally finishes.
	// Without it, every long-sleep/signal run terminated silently. Nil
	// disables the hook (tests that only assert run status).
	OnTerminal func(ctx context.Context, ev TerminalInfo)

	// Cancels registers each resumed run's cancel func so an operator can
	// stop a run that woke from a sleep/signal and is executing again.
	// Optional; nil leaves resumed runs uninterruptible mid-step.
	Cancels *cancelreg.Registry

	// Enqueue switches resume from in-process to re-enqueue (distributed
	// mode): a due schedule flips its run back to "queued" for a worker to
	// claim + resume, instead of the scheduler running the supervisor
	// itself. Default false keeps single-node resume in-process.
	Enqueue bool

	once sync.Once
	stop chan struct{}
}

// TerminalInfo is the payload OnTerminal receives when a resumed run
// finishes. Mirrors dispatcher.TerminalEvent's fields but is declared
// here to avoid an import cycle (dispatcher imports supervisor).
type TerminalInfo struct {
	RunID        string
	WorkflowID   string
	WorkflowSlug string
	Status       string
	TriggerKind  string
	ErrorText    string
	DryRun       bool
}

// Run blocks, ticking the scheduler at TickInterval until ctx is cancelled
// or Stop is called. Safe for one Run per Scheduler.
func (s *Scheduler) Run(ctx context.Context) error {
	s.applyDefaults()
	t := time.NewTicker(s.TickInterval)
	defer t.Stop()

	// Tick once immediately so a fresh process picks up any past-due
	// schedules without waiting for the first interval.
	if err := s.Tick(ctx); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stop:
			return nil
		case <-t.C:
			if err := s.Tick(ctx); err != nil {
				s.Log.Error("scheduler tick failed", "err", err)
			}
		}
	}
}

// Stop signals Run to return. Idempotent.
func (s *Scheduler) Stop() {
	s.once.Do(func() {
		if s.stop != nil {
			close(s.stop)
		}
	})
}

// Tick runs one pass: find due schedules and dispatch each. Exposed so tests
// can drive the scheduler one step at a time without spinning the ticker.
func (s *Scheduler) Tick(ctx context.Context) error {
	s.applyDefaults()
	due, err := s.Journal.FindDueSchedules(ctx, s.Now(), s.Batch)
	if err != nil {
		return err
	}
	for _, sched := range due {
		if err := s.dispatch(ctx, sched); err != nil {
			s.Log.Error("scheduler dispatch failed",
				"schedule_id", sched.ID, "run_id", sched.RunID, "err", err)
		}
	}
	return nil
}

// dispatch handles one due schedule. Sleep + signal kinds resume the same
// way: mark fired, restore run status, re-spawn a Supervisor with the same
// RunID. The new subprocess replays from the journal cache up to the
// suspending frame, where the supervisor (signal: payload or expired
// already in the schedules row; sleep: wake_at past) immediately acks
// and lets the workflow proceed.
func (s *Scheduler) dispatch(ctx context.Context, sched journal.Schedule) error {
	// The schedules row only carries run_id, so resolve the run to find
	// its workflow (for the binary lookup + secret ACL) and its original
	// trigger input (so the resumed process decodes the same Input the
	// first spawn saw). Both were dropped by the old code, which called
	// BinaryPath("") with an empty slug and never restored Input.
	run, err := s.Journal.GetRun(ctx, sched.RunID)
	if err != nil {
		return fmt.Errorf("scheduler: get run %s: %w", sched.RunID, err)
	}

	// Distributed mode: don't resume in-process. Claim the schedule (CAS)
	// and re-enqueue the run so a worker picks it up. The worker's
	// supervisor replays from the journal and the now-fired schedule row
	// (FindLatestSleepSchedule still finds it; only FindDueSchedules
	// filters fired) acks the past-due sleep/signal and resumes.
	if s.Enqueue {
		claimed, err := s.Journal.ClaimSchedule(ctx, sched.ID)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		if err := s.Journal.SetRunStatus(ctx, sched.RunID, "queued"); err != nil {
			return err
		}
		s.Log.Info("scheduler: re-enqueued resumed run for a worker",
			"run_id", sched.RunID, "step", sched.StepName, "kind", sched.Kind)
		return nil
	}

	// A missing workflows row (deleted out from under a suspended run) is
	// non-fatal here: fall back to an empty slug and let BinaryPath decide.
	// In production reg.BinaryPath("") errors and the run surfaces as
	// failed; tests stub BinaryPath to ignore the slug.
	slug, slugErr := s.Journal.WorkflowSlugByID(ctx, run.WorkflowID)
	if slugErr != nil {
		s.Log.Warn("scheduler: resolve workflow slug failed; resuming with empty slug",
			"run_id", sched.RunID, "workflow_id", run.WorkflowID, "err", slugErr)
		slug = ""
	}

	binary, err := s.BinaryPath(slug)
	if err != nil {
		return fmt.Errorf("scheduler: binary lookup for %q: %w", slug, err)
	}

	// Claim the schedule atomically AFTER the binary resolves, so a row we
	// can't service yet isn't consumed. ClaimSchedule is a compare-and-set:
	// claimed=false means a peer daemon already took it, so skip silently.
	claimed, err := s.Journal.ClaimSchedule(ctx, sched.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	// Restore run state so the new supervisor doesn't see "suspended". This is
	// a GUARDED compare-and-set: only respawn if the run is still 'suspended'.
	// A concurrent operator cancel (RequestRunCancel) that committed after
	// ClaimSchedule would otherwise be silently overwritten and the cancelled
	// run would resume and run to completion.
	resumed, err := s.Journal.ResumeSuspendedRun(ctx, sched.RunID)
	if err != nil {
		return err
	}
	if !resumed {
		if s.Log != nil {
			s.Log.Info("scheduler: run no longer suspended (cancelled or finalized); skipping resume", "run_id", sched.RunID)
		}
		return nil
	}

	sup := s.SupervisorTemplate
	sup.BinaryPath = binary
	sup.WorkflowSlug = slug
	sup.RunID = sched.RunID
	sup.Input = run.TriggerMeta
	sup.Journal = s.Journal
	sup.Vault = s.Vault
	if sup.Log == nil {
		sup.Log = s.Log
	}
	if sup.Now == nil {
		sup.Now = s.Now
	}
	if sup.Mode == "" {
		sup.Mode = "live"
	}

	// Run on a cancellable context decoupled from the tick, registered so
	// an operator can stop a resumed run mid-step (exec.CommandContext
	// kills the subprocess on cancel).
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Cancels.Register(sched.RunID, cancel)
	defer s.Cancels.Deregister(sched.RunID)

	status, runErr := sup.Run(runCtx)
	if runCtx.Err() == context.Canceled {
		status = "cancelled"
		runErr = nil
		// FinalizeCancel (not MarkRunFinished) so a schedule row this
		// resumed run wrote on its way to re-suspending is fired too;
		// otherwise the next tick would resume the run we just cancelled.
		if mErr := s.Journal.FinalizeCancel(context.Background(), sched.RunID); mErr != nil {
			s.Log.Warn("scheduler: finalize cancel failed", "run_id", sched.RunID, "err", mErr)
		}
	}
	s.Log.Info("scheduler woke run",
		"run_id", sched.RunID, "step", sched.StepName, "kind", sched.Kind, "status", status)

	// Fire the terminal hook for resumed runs that actually finished. A
	// re-suspend (status "suspended") is not terminal, so notifications +
	// chains wait for the eventual real terminal status.
	if s.OnTerminal != nil && status != "" && status != "suspended" {
		errText := ""
		if runErr != nil {
			errText = runErr.Error()
		}
		s.OnTerminal(ctx, TerminalInfo{
			RunID:        sched.RunID,
			WorkflowID:   run.WorkflowID,
			WorkflowSlug: slug,
			Status:       status,
			TriggerKind:  run.TriggerKind,
			ErrorText:    errText,
			DryRun:       sup.Mode == "dry_run",
		})
	}
	return runErr
}

func (s *Scheduler) applyDefaults() {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.TickInterval == 0 {
		s.TickInterval = 5 * time.Second
	}
	if s.Batch == 0 {
		s.Batch = 50
	}
	if s.stop == nil {
		s.stop = make(chan struct{})
	}
}

// ErrNoBinary is returned by BinaryPath when no workflow binary is registered.
// Surfaces clearly in scheduler logs.
var ErrNoBinary = errors.New("scheduler: no workflow binary registered")
