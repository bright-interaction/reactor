package runlogs

import (
	"sync"
	"testing"
	"time"
)

func TestAppendAndSnapshot(t *testing.T) {
	t.Parallel()
	b := New(10, time.Minute)
	b.Append("run_a", "first")
	b.Append("run_a", "second")
	b.Append("run_b", "other")
	got := b.Snapshot("run_a")
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("snapshot mismatch: %v", got)
	}
	if otherGot := b.Snapshot("run_b"); len(otherGot) != 1 || otherGot[0] != "other" {
		t.Fatalf("run_b leak / loss: %v", otherGot)
	}
}

func TestRingDropsOldestWhenFull(t *testing.T) {
	t.Parallel()
	b := New(3, time.Minute)
	for i, line := range []string{"a", "b", "c", "d", "e"} {
		_ = i
		b.Append("run_x", line)
	}
	got := b.Snapshot("run_x")
	if len(got) != 3 || got[0] != "c" || got[2] != "e" {
		t.Fatalf("ring drop wrong: %v", got)
	}
}

func TestSubscribeReceivesNewLines(t *testing.T) {
	t.Parallel()
	b := New(10, time.Minute)
	b.Append("run_y", "before-subscribe")
	sub := b.Subscribe("run_y")
	defer b.Unsubscribe("run_y", sub)

	go func() {
		time.Sleep(10 * time.Millisecond)
		b.Append("run_y", "after-subscribe")
	}()

	select {
	case got := <-sub:
		if got != "after-subscribe" {
			t.Fatalf("got %q, want after-subscribe", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber timed out")
	}
}

func TestCloseClosesAllSubscribers(t *testing.T) {
	t.Parallel()
	b := New(10, time.Minute)
	sub := b.Subscribe("run_z")
	b.Close("run_z")
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("channel should be closed after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("close didn't propagate to subscriber")
	}
}

func TestSubscribeAfterCloseClosesImmediately(t *testing.T) {
	t.Parallel()
	b := New(10, time.Minute)
	b.Append("run_w", "line")
	b.Close("run_w")
	sub := b.Subscribe("run_w")
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("late subscriber should see closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("late subscribe to closed run did not close immediately")
	}
}

func TestConcurrentAppendsAreSafe(t *testing.T) {
	t.Parallel()
	b := New(1000, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Append("run_concurrent", "line")
			}
		}()
	}
	wg.Wait()
	if got := len(b.Snapshot("run_concurrent")); got != 1000 {
		t.Fatalf("got %d lines, want 1000", got)
	}
}
