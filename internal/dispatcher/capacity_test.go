package dispatcher

import "testing"

// TestAcquireReleaseCap proves the concurrency cap is enforced: once
// MaxConcurrent slots are taken, acquire fails fast (load shedding)
// until a slot is released.
func TestAcquireReleaseCap(t *testing.T) {
	d := &Dispatcher{MaxConcurrent: 2}
	if !d.acquire() || !d.acquire() {
		t.Fatal("first two acquires should succeed")
	}
	if d.acquire() {
		t.Fatal("third acquire should fail at capacity")
	}
	d.release()
	if !d.acquire() {
		t.Fatal("acquire should succeed after a release")
	}
}

// TestAcquireUnlimited confirms a zero cap means unlimited (back-compat
// + test default): acquire always succeeds and release is a no-op.
func TestAcquireUnlimited(t *testing.T) {
	d := &Dispatcher{} // MaxConcurrent == 0
	for i := 0; i < 100; i++ {
		if !d.acquire() {
			t.Fatalf("unlimited dispatcher should never block (i=%d)", i)
		}
	}
	d.release() // no-op, must not panic
}
