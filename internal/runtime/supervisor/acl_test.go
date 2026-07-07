package supervisor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bright-interaction/reactor/internal/migrate"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
	"github.com/bright-interaction/reactor/internal/runtime/wire"
	"github.com/bright-interaction/reactor/internal/vault"
	_ "modernc.org/sqlite"
)

// newACLEnv builds a journal + vault + a credential that the dispatcher
// can try to fetch. Workflow is created via the journal; whether the
// (workflow, credential) grant exists is the test's choice.
func newACLEnv(t *testing.T) (*journal.Journal, *vault.Store) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "acl.db")
	url := "sqlite://" + dbPath

	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := migrate.Up(context.Background(), silent, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	j := journal.New(db, journal.EngineSQLite)
	if err := j.CreateWorkflow(context.Background(), "wf_acl", "acl-test", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("create wf: %v", err)
	}

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = 0xCD
	}
	v, err := vault.NewStore(vault.NewMemoryBackend(), masterKey)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	if err := v.Put(context.Background(), "cred_secret", []byte("the-plaintext")); err != nil {
		t.Fatalf("vault put: %v", err)
	}
	return j, v
}

// runHandleSecretFetch constructs a dispatcher with the given ACL
// permissiveness, invokes handleSecretFetch with a SecretFetch frame
// for cred_secret against workflow slug acl-test, and returns the
// decoded SecretReply that came out of the dispatcher's writer.
func runHandleSecretFetch(t *testing.T, j *journal.Journal, v *vault.Store, permissive bool) wire.SecretReply {
	t.Helper()
	sup := &Supervisor{
		WorkflowSlug:  "acl-test",
		RunID:         "run_acl",
		Journal:       j,
		Vault:         v,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		ACLPermissive: permissive,
	}
	var buf bytes.Buffer
	disp := &dispatcher{
		sup:     sup,
		enc:     wire.NewEncoder(&buf),
		writeMu: &sync.Mutex{},
	}

	req, err := wire.Wrap(1, 0, wire.KindSecretFetch, wire.SecretFetch{ID: "cred_secret"})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if err := disp.handleSecretFetch(context.Background(), req); err != nil {
		t.Fatalf("handleSecretFetch: %v", err)
	}

	dec := wire.NewDecoder(&buf)
	reply, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	var sr wire.SecretReply
	if err := wire.Unwrap(reply, &sr); err != nil {
		t.Fatalf("unwrap reply: %v", err)
	}
	return sr
}

func TestSecretFetchEmptyACLStrictDeniesByDefault(t *testing.T) {
	t.Parallel()
	j, v := newACLEnv(t)

	reply := runHandleSecretFetch(t, j, v, false)
	if !reply.NotFound {
		t.Fatalf("strict mode + empty grants table should deny; got reply=%+v", reply)
	}
	if len(reply.Value) != 0 {
		t.Fatalf("strict mode leaked plaintext: %q", reply.Value)
	}
}

func TestSecretFetchEmptyACLPermissiveAllows(t *testing.T) {
	t.Parallel()
	j, v := newACLEnv(t)

	reply := runHandleSecretFetch(t, j, v, true)
	if reply.NotFound {
		t.Fatalf("permissive mode should allow on empty table; got NotFound")
	}
	if string(reply.Value) != "the-plaintext" {
		t.Fatalf("plaintext mismatch: got %q want %q", reply.Value, "the-plaintext")
	}
}

func TestSecretFetchExplicitGrantAllowsRegardlessOfPermissive(t *testing.T) {
	t.Parallel()
	j, v := newACLEnv(t)
	if err := j.GrantSecret(context.Background(), "wf_acl", "cred_secret", "test", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}

	reply := runHandleSecretFetch(t, j, v, false)
	if reply.NotFound {
		t.Fatalf("explicit grant should allow under strict mode; got NotFound")
	}
	if string(reply.Value) != "the-plaintext" {
		t.Fatalf("plaintext mismatch: got %q want %q", reply.Value, "the-plaintext")
	}
}

func TestSecretFetchNonEmptyACLWithoutGrantDenies(t *testing.T) {
	t.Parallel()
	j, v := newACLEnv(t)
	// Seed an unrelated grant so the table is non-empty; this
	// disables the empty-table fallback for both modes.
	if err := j.GrantSecret(context.Background(), "wf_acl", "cred_other", "test", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}

	for _, permissive := range []bool{false, true} {
		reply := runHandleSecretFetch(t, j, v, permissive)
		if !reply.NotFound {
			t.Fatalf("permissive=%v: missing grant should deny even on non-empty table; got %+v", permissive, reply)
		}
	}
}
