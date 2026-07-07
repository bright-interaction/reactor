package credentials

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/brightinteraction/reactor/internal/migrate"
	_ "modernc.org/sqlite"
)

func newRepo(t *testing.T) (*Repo, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "c.db")
	url := "sqlite://" + dbPath

	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := migrate.Up(context.Background(), silent, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return New(db, EngineSQLite), func() { db.Close() }
}

func TestCreateAndGet(t *testing.T) {
	t.Parallel()
	repo, cleanup := newRepo(t)
	defer cleanup()

	id, _ := NewID()
	if err := repo.Create(context.Background(), CreateParams{
		ID:                   id,
		Name:                 "demo-cf-token",
		Service:              "cloudflare",
		Provider:             "cloudflare",
		ProviderMeta:         map[string]string{"zone": "z1"},
		AutoRotate:           true,
		RotationIntervalDays: 30,
		RotationTargets: []Target{
			{Kind: "webhook", URL: "https://x", SecretID: "cred_hmac", KeyName: "CF_TOKEN"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	c, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "demo-cf-token" || c.Provider != "cloudflare" || !c.AutoRotate ||
		c.RotationIntervalDays != 30 || len(c.RotationTargets) != 1 ||
		c.ProviderMeta["zone"] != "z1" {
		t.Fatalf("shape mismatch: %+v", c)
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	repo, cleanup := newRepo(t)
	defer cleanup()
	if _, err := repo.Get(context.Background(), "cred_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestListNeedingRotation(t *testing.T) {
	t.Parallel()
	repo, cleanup := newRepo(t)
	defer cleanup()
	ctx := context.Background()

	idAuto, _ := NewID()
	idManual, _ := NewID()

	if err := repo.Create(ctx, CreateParams{
		ID: idAuto, Name: "a", Service: "x", Provider: "shared-secret",
		AutoRotate: true, RotationIntervalDays: 30,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, CreateParams{
		ID: idManual, Name: "b", Service: "x", Provider: "manual",
		AutoRotate: false, RotationIntervalDays: 30,
	}); err != nil {
		t.Fatal(err)
	}

	due, err := repo.ListNeedingRotation(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != idAuto {
		t.Fatalf("expected [auto], got %d entries", len(due))
	}

	// After MarkRotated within the interval, no longer due.
	if err := repo.MarkRotated(ctx, idAuto, time.Now()); err != nil {
		t.Fatal(err)
	}
	due, err = repo.ListNeedingRotation(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("expected none due after rotate, got %d", len(due))
	}

	// Past the interval, due again.
	due, err = repo.ListNeedingRotation(ctx, time.Now().Add(31*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due past interval, got %d", len(due))
	}
}

func TestRecordErrorAndClear(t *testing.T) {
	t.Parallel()
	repo, cleanup := newRepo(t)
	defer cleanup()
	ctx := context.Background()

	id, _ := NewID()
	if err := repo.Create(ctx, CreateParams{ID: id, Name: "n", Service: "s", Provider: "shared-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordError(ctx, id, "boom"); err != nil {
		t.Fatal(err)
	}
	c, _ := repo.Get(ctx, id)
	if c.LastRotationError != "boom" {
		t.Fatalf("error not stamped: %q", c.LastRotationError)
	}
	if err := repo.MarkRotated(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	c, _ = repo.Get(ctx, id)
	if c.LastRotationError != "" {
		t.Fatalf("error not cleared: %q", c.LastRotationError)
	}
}

func TestAuditAppendAndList(t *testing.T) {
	t.Parallel()
	repo, cleanup := newRepo(t)
	defer cleanup()
	ctx := context.Background()

	id, _ := NewID()
	if err := repo.Create(ctx, CreateParams{ID: id, Name: "n", Service: "s", Provider: "shared-secret"}); err != nil {
		t.Fatal(err)
	}
	for i, action := range []string{"rotate.start", "rotate.success", "rotate.delivery_success"} {
		if err := repo.AppendAudit(ctx, AuditEntry{
			CredentialID: id,
			Action:       action,
			ActorKind:    "scheduler",
			Detail:       json.RawMessage(`{"i":"test"}`),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	rows, err := repo.ListAudit(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d audit rows, want 3", len(rows))
	}
	// Newest-first ordering.
	if rows[0].Action != "rotate.delivery_success" {
		t.Fatalf("expected newest first, got %s", rows[0].Action)
	}
}
