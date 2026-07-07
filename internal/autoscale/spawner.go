// Package autoscale grows and shrinks the distributed-mode worker pool to
// match queue depth. The Spawner abstracts HOW a worker is started so the
// same controller drives every substrate unchanged -- it only ever calls
// Spawn/Stop/Running. Two implementations ship: ProcessSpawner (same-host
// child processes, the default) and CommandSpawner (runs the operator's own
// docker/kubectl/nomad CLI, so Docker + Kubernetes + anything else work with
// zero added dependencies).
package autoscale

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// Spawner starts and stops worker instances. Implementations own the
// lifecycle of whatever they spawn (process, container, pod) and report
// how many they are currently running.
type Spawner interface {
	// Spawn starts one worker and returns an id the controller can Stop.
	Spawn(ctx context.Context) (id string, err error)
	// Stop asks the worker with id to drain + exit (graceful).
	Stop(ctx context.Context, id string) error
	// Running is how many workers this spawner currently manages.
	Running() int
	// IDs lists the managed worker ids (newest last).
	IDs() []string
	// StopAll drains every managed worker (called on shutdown).
	StopAll(ctx context.Context)
}

// ProcessSpawner runs workers as child OS processes on the same host. It is
// the simplest, lowest-blast-radius substrate: bounded by one machine's
// resources, no orchestrator dependency, no cloud cost. Stop sends SIGTERM
// so the worker drains its in-flight runs before exiting.
type ProcessSpawner struct {
	// Binary is the path to the reactor executable (os.Executable()).
	Binary string
	// Args is the full `worker ...` argv (db, root, concurrency, etc.),
	// excluding the binary itself.
	Args []string
	// Env is the child environment (defaults to the parent's when nil).
	Env []string
	Log *slog.Logger

	mu    sync.Mutex
	procs map[string]*exec.Cmd
	seq   int
}

// NewProcessSpawner builds a same-host process spawner.
func NewProcessSpawner(binary string, args, env []string, log *slog.Logger) *ProcessSpawner {
	if log == nil {
		log = slog.Default()
	}
	return &ProcessSpawner{Binary: binary, Args: args, Env: env, Log: log, procs: map[string]*exec.Cmd{}}
}

// Spawn starts an `reactor worker` child process.
func (p *ProcessSpawner) Spawn(_ context.Context) (string, error) {
	cmd := exec.Command(p.Binary, p.Args...)
	if p.Env != nil {
		cmd.Env = p.Env
	}
	// Inherit stdout/stderr so worker logs surface alongside the daemon's.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("autoscale: spawn worker: %w", err)
	}
	p.mu.Lock()
	p.seq++
	id := fmt.Sprintf("proc-%d-%d", cmd.Process.Pid, p.seq)
	p.procs[id] = cmd
	p.mu.Unlock()
	p.Log.Info("autoscale: spawned worker", "id", id, "pid", cmd.Process.Pid)
	// Reap on exit so Running() reflects reality even if a worker dies on
	// its own (crash, OOM) rather than via Stop.
	go func() {
		_ = cmd.Wait()
		p.mu.Lock()
		delete(p.procs, id)
		p.mu.Unlock()
		p.Log.Info("autoscale: worker exited", "id", id)
	}()
	return id, nil
}

// Stop signals a managed worker to drain + exit.
func (p *ProcessSpawner) Stop(_ context.Context, id string) error {
	p.mu.Lock()
	cmd := p.procs[id]
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	p.Log.Info("autoscale: stopping worker", "id", id)
	return cmd.Process.Signal(syscall.SIGTERM)
}

// Running returns the count of managed child processes.
func (p *ProcessSpawner) Running() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.procs)
}

// IDs returns the managed worker ids.
func (p *ProcessSpawner) IDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.procs))
	for id := range p.procs {
		out = append(out, id)
	}
	return out
}

// StopAll drains every managed worker.
func (p *ProcessSpawner) StopAll(ctx context.Context) {
	for _, id := range p.IDs() {
		_ = p.Stop(ctx, id)
	}
}
