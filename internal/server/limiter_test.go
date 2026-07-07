package server

import (
	"testing"
	"time"
)

func TestIPLimiterEvictsIdleBuckets(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(10, 1)
	// Make idle eviction quick + sweep cadence equally quick so the
	// test does not need to wall-clock-sleep for a minute.
	l.idleEvictAge = 10 * time.Millisecond

	// Seed buckets with a request from each IP.
	for i := 0; i < 100; i++ {
		l.allow(testIP(i))
	}
	if got := len(l.buckets); got != 100 {
		t.Fatalf("seeded buckets = %d, want 100", got)
	}

	// Reach inside the limiter to force a sweep without waiting for
	// the production 1-minute throttle. Mark every bucket as last-
	// seen-long-ago, then call allow once with a fresh IP.
	l.mu.Lock()
	for _, b := range l.buckets {
		b.last = time.Now().Add(-time.Hour)
	}
	l.lastSweep = time.Now().Add(-time.Hour)
	l.mu.Unlock()
	l.allow(testIP(999))

	l.mu.Lock()
	left := len(l.buckets)
	l.mu.Unlock()
	if left > 1 {
		t.Fatalf("after sweep, buckets = %d, want 1 (only the fresh entry)", left)
	}
}

func TestIPLimiterMaxBucketsCeiling(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(10, 1)
	l.maxBuckets = 5
	for i := 0; i < 5; i++ {
		if !l.allow(testIP(i)) {
			t.Fatalf("ip %d: allow returned false unexpectedly", i)
		}
	}
	if got := l.allow(testIP(99)); got {
		t.Fatalf("over-cap IP allowed; want refusal")
	}
}

func testIP(i int) string {
	return time.Now().Format("2006") + "::" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
