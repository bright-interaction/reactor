package autoscale

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeSpawner struct {
	mu  sync.Mutex
	ids []string
	seq int
}

func (f *fakeSpawner) Spawn(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("w%d", f.seq)
	f.ids = append(f.ids, id)
	return id, nil
}
func (f *fakeSpawner) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.ids {
		if x == id {
			f.ids = append(f.ids[:i], f.ids[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeSpawner) Running() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.ids) }
func (f *fakeSpawner) IDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}
func (f *fakeSpawner) StopAll(ctx context.Context) {
	for _, id := range f.IDs() {
		_ = f.Stop(ctx, id)
	}
}

type fakeDemand struct{ queued int }

func (d *fakeDemand) CountQueued(context.Context) (int, error) { return d.queued, nil }

func TestControllerScalesUpGraduallyToMaxThenDown(t *testing.T) {
	sp := &fakeSpawner{}
	dm := &fakeDemand{}
	ctrl := New(Config{Min: 0, Max: 3, QueuePerWorker: 20, ScaleUpCooldown: time.Minute, ScaleDownCooldown: time.Minute}, sp, dm, nil)
	now := time.Unix(1_700_000_000, 0)
	ctrl.now = func() time.Time { return now }
	ctrl.lastScaleUp = now.Add(-time.Hour) // cooldown already elapsed
	ctx := context.Background()

	// 100 queued -> desired ceil(100/20)=5, clamped to Max=3, but scale-up
	// is ONE worker per tick gated by the cooldown.
	dm.queued = 100
	ctrl.tick(ctx)
	if sp.Running() != 1 {
		t.Fatalf("first tick: running=%d, want 1 (gradual)", sp.Running())
	}
	// Immediate re-tick is blocked by the scale-up cooldown.
	ctrl.tick(ctx)
	if sp.Running() != 1 {
		t.Fatalf("cooldown should block a second spawn, running=%d", sp.Running())
	}
	// Advance past cooldown each tick -> 2, then 3 (Max), then capped.
	now = now.Add(2 * time.Minute)
	ctrl.tick(ctx)
	now = now.Add(2 * time.Minute)
	ctrl.tick(ctx)
	if sp.Running() != 3 {
		t.Fatalf("running=%d, want 3 (Max)", sp.Running())
	}
	now = now.Add(2 * time.Minute)
	ctrl.tick(ctx)
	if sp.Running() != 3 {
		t.Fatalf("must not exceed Max, running=%d", sp.Running())
	}

	// Queue drains. After the scale-down cooldown (queue empty) workers are
	// removed one per tick down to Min=0.
	dm.queued = 0
	now = now.Add(2 * time.Minute) // lastBusy is now stale by > cooldown
	ctrl.tick(ctx)
	if sp.Running() != 2 {
		t.Fatalf("scale down: running=%d, want 2", sp.Running())
	}
	now = now.Add(2 * time.Minute)
	ctrl.tick(ctx)
	now = now.Add(2 * time.Minute)
	ctrl.tick(ctx)
	if sp.Running() != 0 {
		t.Fatalf("should scale to zero (Min=0), running=%d", sp.Running())
	}
}

func TestControllerRespectsMinFloor(t *testing.T) {
	sp := &fakeSpawner{}
	ctrl := New(Config{Min: 2, Max: 4, ScaleDownCooldown: time.Second}, sp, &fakeDemand{queued: 0}, nil)
	now := time.Unix(1_700_000_000, 0)
	ctrl.now = func() time.Time { return now }
	// desiredWorkers(0) must be the Min floor, never below.
	if d := ctrl.desiredWorkers(0); d != 2 {
		t.Fatalf("desired(0)=%d, want Min=2", d)
	}
	if d := ctrl.desiredWorkers(1000); d != 4 {
		t.Fatalf("desired(1000)=%d, want Max=4", d)
	}
}
