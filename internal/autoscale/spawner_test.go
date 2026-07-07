package autoscale

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"testing"
	"time"
)

func TestProcessSpawnerSpawnStopReap(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary on PATH")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp := NewProcessSpawner(sleep, []string{"30"}, nil, log)

	id, err := sp.Spawn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sp.Running() != 1 {
		t.Fatalf("after spawn: running=%d, want 1", sp.Running())
	}
	if err := sp.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Stop is async (SIGTERM); the reap goroutine removes the entry when the
	// process exits. Poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	for sp.Running() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sp.Running() != 0 {
		t.Fatalf("worker not reaped after Stop, running=%d", sp.Running())
	}
}
