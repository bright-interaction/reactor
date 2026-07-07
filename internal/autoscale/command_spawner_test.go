package autoscale

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCommandSpawnerUsesStdoutAsID(t *testing.T) {
	sp := NewCommandSpawner([]string{"printf", "container-abc123"}, []string{"true"}, nil, quietLog())
	id, err := sp.Spawn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id != "container-abc123" {
		t.Fatalf("id = %q, want container-abc123", id)
	}
	if sp.Running() != 1 {
		t.Fatalf("running = %d, want 1", sp.Running())
	}
	if got := sp.IDs(); len(got) != 1 || got[0] != "container-abc123" {
		t.Fatalf("ids = %v", got)
	}
}

func TestCommandSpawnerSynthesizesIDWhenNoStdout(t *testing.T) {
	sp := NewCommandSpawner([]string{"true"}, []string{"true"}, nil, quietLog())
	id, err := sp.Spawn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id != "cmd-1" {
		t.Fatalf("synthesized id = %q, want cmd-1", id)
	}
}

func TestCommandSpawnerStopUntracksOnSuccess(t *testing.T) {
	sp := NewCommandSpawner([]string{"printf", "abc"}, []string{"true"}, nil, quietLog())
	id, _ := sp.Spawn(context.Background())
	if err := sp.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if sp.Running() != 0 {
		t.Fatalf("running after stop = %d, want 0", sp.Running())
	}
}

func TestCommandSpawnerStopKeepsTrackedOnFailure(t *testing.T) {
	// `false` exits non-zero: the worker stays tracked so the next tick retries
	// rather than silently leaking the container.
	sp := NewCommandSpawner([]string{"printf", "abc"}, []string{"false"}, nil, quietLog())
	id, _ := sp.Spawn(context.Background())
	if err := sp.Stop(context.Background(), id); err == nil {
		t.Fatal("expected stop to return the command error")
	}
	if sp.Running() != 1 {
		t.Fatalf("running after failed stop = %d, want 1 (kept for retry)", sp.Running())
	}
}

func TestCommandSpawnerSubstitutesID(t *testing.T) {
	got := substituteID([]string{"docker", "stop", "{id}"}, "deadbeef")
	if len(got) != 3 || got[2] != "deadbeef" {
		t.Fatalf("substituteID = %v", got)
	}
	// The original argv must not be mutated (it is reused for every Stop).
	orig := []string{"kubectl", "delete", "{id}"}
	_ = substituteID(orig, "pod/x")
	if orig[2] != "{id}" {
		t.Fatalf("substituteID mutated the input: %v", orig)
	}
}

func TestCommandSpawnerEmptySpawnArgvErrors(t *testing.T) {
	sp := NewCommandSpawner(nil, nil, nil, quietLog())
	if _, err := sp.Spawn(context.Background()); err == nil {
		t.Fatal("expected an error for an empty spawn command")
	}
}

func TestCommandSpawnerStopAllDrains(t *testing.T) {
	sp := NewCommandSpawner([]string{"printf", "x"}, []string{"true"}, nil, quietLog())
	// printf "x" yields the same id each time; track three distinct via seq is
	// not the goal here -- just verify StopAll empties the set.
	_, _ = sp.Spawn(context.Background())
	sp.StopAll(context.Background())
	if sp.Running() != 0 {
		t.Fatalf("running after StopAll = %d, want 0", sp.Running())
	}
}
