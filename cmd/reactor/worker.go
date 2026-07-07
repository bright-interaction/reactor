package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// cmdWorker runs a distributed-mode worker: it claims queued runs from the
// shared Postgres journal (FOR UPDATE SKIP LOCKED), executes each through
// the dispatcher's ExecuteRun, heartbeats the lease while running, and
// reaps leases from dead workers. Run as many as you want against the same
// --db; they share the queue safely. This is how Reactor "duplicates
// itself" to add capacity -- each copy is just another `reactor worker`.
//
//	reactor worker --db postgres://... --root <dir> [--concurrency N]
func cmdWorker(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "database URL (must be postgres:// for distributed mode)")
	root := fs.String("root", defaultRoot(), "Reactor state directory (workflows/ + master.key)")
	masterKeyFile := fs.String("master-key-file", "", "path to a 64-hex-char master key (default <root>/master.key)")
	masterKeyHex := fs.String("master-key", envFirst("REACTOR_MASTER_KEY", "ARACHNE_MASTER_KEY"), "32-byte hex master key (overrides --master-key-file)")
	aclPermissive := fs.Bool("vault-acl-permissive", os.Getenv("REACTOR_VAULT_ACL_PERMISSIVE") == "1", "treat an empty workflow_secret_grants table as allow-all (legacy)")
	cgroupRoot := fs.String("cgroup-root", os.Getenv("REACTOR_CGROUP_ROOT"), "cgroup v2 root for per-run memory.max + pids.max caps")
	concurrency := fs.Int("concurrency", envIntFirst("REACTOR_WORKER_CONCURRENCY", "", runtime.NumCPU()), "max runs this worker executes at once")
	leaseTTL := fs.Duration("lease-ttl", 60*time.Second, "how long a claimed run's lease is valid before a reaper may requeue it (heartbeated while running)")
	pollInterval := fs.Duration("poll-interval", 2*time.Second, "how often to poll for queued work when idle")
	drainTimeout := fs.Duration("drain-timeout", time.Duration(envIntFirst("REACTOR_DRAIN_TIMEOUT", "ARACHNE_DRAIN_TIMEOUT", 30))*time.Second, "max time to wait for in-flight runs on shutdown")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("worker: missing --db (or $REACTOR_DB_URL)")
	}
	if !isPostgresURL(*dbURL) {
		return errors.New("worker: distributed mode requires a postgres:// database")
	}
	if *concurrency < 1 {
		*concurrency = 1
	}
	masterHex, err := loadMasterKey(*masterKeyHex, *masterKeyFile, *root)
	if err != nil {
		return err
	}
	masterKey, err := decodeMasterKey(masterHex)
	if err != nil {
		return err
	}

	cfg := &serveConfig{
		dbURL:            *dbURL,
		root:             *root,
		masterKey:        masterKey,
		aclPermissive:    *aclPermissive,
		cgroupRoot:       *cgroupRoot,
		tickInterval:     5 * time.Second,
		rotationInterval: time.Hour,
		cronReload:       30 * time.Second,
		drainTimeout:     *drainTimeout,
		mode:             "distributed",
	}
	deps, err := openServeDeps(ctx, log, cfg)
	if err != nil {
		return err
	}
	defer deps.db.Close()

	return runWorkerLoop(ctx, log, deps, workerOpts{
		concurrency:  *concurrency,
		leaseTTL:     *leaseTTL,
		pollInterval: *pollInterval,
		drainTimeout: *drainTimeout,
	})
}

type workerOpts struct {
	concurrency  int
	leaseTTL     time.Duration
	pollInterval time.Duration
	drainTimeout time.Duration
}

func runWorkerLoop(ctx context.Context, log *slog.Logger, deps *serveDeps, opts workerOpts) error {
	workerID := genWorkerID()
	log.Info("worker: starting", "worker_id", workerID, "concurrency", opts.concurrency, "lease_ttl", opts.leaseTTL.String())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Info("worker: signal received, shutting down", "signal", s.String())
		cancel()
	}()

	sem := make(chan struct{}, opts.concurrency)
	var wg sync.WaitGroup

	// Register in the worker heartbeat table so the dashboard fleet view +
	// autoscaler see this worker; refresh while running; remove on exit.
	_ = deps.journal.UpsertWorkerHeartbeat(runCtx, workerID, opts.concurrency)
	defer func() {
		_ = deps.journal.DeleteWorker(context.Background(), workerID)
	}()
	go heartbeatWorker(runCtx, deps, workerID, opts.concurrency)

	// Background: requeue dead workers' runs, and honour cross-process
	// cancel requests against this worker's live runs.
	go reaperLoop(runCtx, log, deps, opts.leaseTTL)
	go runCancelWatcher(runCtx, log, deps)

	idle := time.NewTimer(opts.pollInterval)
	defer idle.Stop()
	for runCtx.Err() == nil {
		free := cap(sem) - len(sem)
		if free <= 0 {
			// All slots busy; wait briefly for one to free.
			select {
			case <-runCtx.Done():
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		ids, err := deps.journal.ClaimQueuedRuns(runCtx, workerID, free, opts.leaseTTL)
		if err != nil {
			if runCtx.Err() != nil {
				break
			}
			log.Warn("worker: claim failed", "err", err)
			time.Sleep(opts.pollInterval)
			continue
		}
		if len(ids) == 0 {
			// Idle: wait a poll interval (or until shutdown) before retrying.
			idle.Reset(opts.pollInterval)
			select {
			case <-runCtx.Done():
			case <-idle.C:
			}
			continue
		}
		for _, id := range ids {
			sem <- struct{}{}
			wg.Add(1)
			go func(runID string) {
				defer wg.Done()
				defer func() { <-sem }()
				stopHB := make(chan struct{})
				go heartbeatLease(deps, runID, workerID, opts.leaseTTL, stopHB)
				if err := deps.dispatcher.ExecuteRun(context.Background(), runID); err != nil {
					log.Error("worker: execute failed", "run_id", runID, "err", err)
				}
				close(stopHB)
			}(id)
		}
	}

	log.Info("worker: draining in-flight runs", "count", deps.dispatcher.InFlight(), "timeout", opts.drainTimeout.String())
	if err := deps.dispatcher.Drain(opts.drainTimeout); err != nil {
		killed := deps.cancels.CancelAll()
		log.Warn("worker: drain timed out; killed in-flight runs", "killed", killed, "err", err)
		_ = deps.dispatcher.Drain(5 * time.Second)
	}
	wg.Wait()
	log.Info("worker: shutdown complete")
	return nil
}

// heartbeatWorker refreshes this worker's row in the registry so the
// fleet view + autoscaler keep seeing it. Stops with ctx.
func heartbeatWorker(ctx context.Context, deps *serveDeps, workerID string, concurrency int) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = deps.journal.UpsertWorkerHeartbeat(ctx, workerID, concurrency)
		}
	}
}

// reaperLoop periodically requeues runs whose worker died (expired lease).
func reaperLoop(ctx context.Context, log *slog.Logger, deps *serveDeps, leaseTTL time.Duration) {
	interval := leaseTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := deps.journal.ReapExpiredLeases(ctx); err != nil {
				log.Warn("worker: reap expired leases failed", "err", err)
			} else if n > 0 {
				log.Info("worker: requeued runs from dead workers", "count", n)
			}
		}
	}
}

// heartbeatLease keeps a claimed run's lease fresh while it executes so the
// reaper doesn't requeue a long-but-healthy run. Stops when stop is closed.
func heartbeatLease(deps *serveDeps, runID, workerID string, leaseTTL time.Duration, stop <-chan struct{}) {
	interval := leaseTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_ = deps.journal.ExtendLease(context.Background(), runID, workerID, leaseTTL)
		}
	}
}

// genWorkerID returns a stable-per-process id: hostname-pid-rand.
func genWorkerID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))
}
