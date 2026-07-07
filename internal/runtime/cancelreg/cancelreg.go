// Package cancelreg is a tiny process-local registry mapping a run id to
// the context.CancelFunc of its executing supervisor. The dispatcher and
// the scheduler both register the runs they execute; the dashboard cancel
// handler and the cross-process cancel watcher call Cancel(runID) to kill
// a live run's subprocess (exec.CommandContext fires SIGKILL when the
// context is cancelled).
package cancelreg

import (
	"context"
	"sync"
)

// Registry is safe for concurrent use.
type Registry struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{m: map[string]context.CancelFunc{}}
}

// Register records the cancel func for a running run. A nil Registry is a
// no-op so callers don't have to nil-check.
func (r *Registry) Register(runID string, cancel context.CancelFunc) {
	if r == nil || runID == "" || cancel == nil {
		return
	}
	r.mu.Lock()
	r.m[runID] = cancel
	r.mu.Unlock()
}

// Deregister drops a run's entry once it finishes.
func (r *Registry) Deregister(runID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.m, runID)
	r.mu.Unlock()
}

// Cancel invokes the run's cancel func if it is executing here. Returns
// true when a live run was cancelled, false when the run is not on this
// daemon (suspended, already finished, or executing elsewhere).
func (r *Registry) Cancel(runID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	cancel, ok := r.m[runID]
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// CancelAll cancels every registered run and returns how many it
// signalled. Used at shutdown when graceful drain times out: killing the
// subprocesses (exec.CommandContext SIGKILL) prevents orphaned children
// and stops them writing to a DB that is about to close. The cancel funcs
// are snapshotted under the lock so callers' Deregister doesn't race.
func (r *Registry) CancelAll() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	funcs := make([]context.CancelFunc, 0, len(r.m))
	for _, c := range r.m {
		funcs = append(funcs, c)
	}
	r.mu.Unlock()
	for _, c := range funcs {
		c()
	}
	return len(funcs)
}
