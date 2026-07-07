package server

import (
	"sync"
	"time"
)

// loginThrottle is an in-memory failed-login limiter. It bounds online
// password brute-forcing: after maxFailures failed attempts for a given
// (ip, username) key the key is locked for lockWindow. Keyed by the pair
// rather than by username alone so one attacker can't lock every account
// out from a single source, and not by IP alone so a shared proxy IP
// doesn't let one brute-forcer wedge unrelated accounts.
//
// In-memory is the right scope for a single-node self-hosted daemon (the
// rate-limit + session stores are in-memory too); a restart clears the
// counters, which only ever loosens the limit.
type loginThrottle struct {
	mu          sync.Mutex
	failures    map[string]*throttleEntry
	maxFailures int
	lockWindow  time.Duration
	now         func() time.Time
}

type throttleEntry struct {
	count       int
	lockedUntil time.Time
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		failures:    map[string]*throttleEntry{},
		maxFailures: 5,
		lockWindow:  15 * time.Minute,
		now:         time.Now,
	}
}

// locked reports whether the key is currently in cooldown.
func (t *loginThrottle) locked(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.failures[key]
	if e == nil {
		return false
	}
	if e.lockedUntil.IsZero() {
		return false
	}
	if t.now().After(e.lockedUntil) {
		// Cooldown elapsed; reset so the next attempt starts fresh.
		delete(t.failures, key)
		return false
	}
	return true
}

// recordFailure bumps the failure count and arms the lock once the
// threshold is crossed.
func (t *loginThrottle) recordFailure(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.failures[key]
	if e == nil {
		e = &throttleEntry{}
		t.failures[key] = e
	}
	e.count++
	if e.count >= t.maxFailures {
		e.lockedUntil = t.now().Add(t.lockWindow)
	}
}

// reset clears the counter for a key after a successful login.
func (t *loginThrottle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, key)
}
