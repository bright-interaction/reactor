package journal

import (
	"context"
	"errors"
	"testing"
)

func TestGrantsEmptyTableIsPermissive(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	if _, err := j.HasGrant(context.Background(), "wf_x", "cred_x"); !errors.Is(err, ErrACLEmpty) {
		t.Fatalf("got %v, want ErrACLEmpty", err)
	}
}

func TestGrantSecretRefusesCrossTenant(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := j.db.ExecContext(ctx, j.bind(q), args...); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	mustExec(`INSERT INTO workflows (id, tenant_id, slug, code_hash, sdk_version, dag_json) VALUES ($1,$2,$3,$4,$5,$6)`,
		"wf_a", "tenant-a", "wf-a", "h", "0.1.0", "{}")
	mustExec(`INSERT INTO credentials (id, tenant_id, name, service, blob) VALUES ($1,$2,$3,$4,$5)`,
		"cred_a", "tenant-a", "ca", "svc", []byte("x"))
	mustExec(`INSERT INTO credentials (id, tenant_id, name, service, blob) VALUES ($1,$2,$3,$4,$5)`,
		"cred_b", "tenant-b", "cb", "svc", []byte("x"))

	// A grant linking tenant-a's workflow to tenant-b's credential is refused.
	if err := j.GrantSecret(ctx, "wf_a", "cred_b", "tester", ""); err == nil {
		t.Fatal("cross-tenant grant (wf tenant-a -> cred tenant-b) should be refused")
	}
	// A same-tenant grant is allowed.
	if err := j.GrantSecret(ctx, "wf_a", "cred_a", "tester", ""); err != nil {
		t.Fatalf("same-tenant grant should be allowed: %v", err)
	}
}

// seedGrantTargets inserts the workflow + credential rows a grant refers to,
// all in one tenant. GrantSecret and RevokeSecret require both sides to exist:
// they used to skip the check when either row was absent, which is what let a
// phantom grant through the MCP tool and the CLI.
func seedGrantTargets(t *testing.T, j *Journal, workflowIDs, credentialIDs []string) {
	t.Helper()
	ctx := context.Background()
	for i, id := range workflowIDs {
		if _, err := j.db.ExecContext(ctx, j.bind(
			`INSERT INTO workflows (id, tenant_id, slug, code_hash, sdk_version, dag_json) VALUES ($1,$2,$3,$4,$5,$6)`),
			id, "default", "slug-"+id, "h", "0.1.0", "{}"); err != nil {
			t.Fatalf("seed workflow %s (%d): %v", id, i, err)
		}
	}
	for _, id := range credentialIDs {
		if _, err := j.db.ExecContext(ctx, j.bind(
			`INSERT INTO credentials (id, tenant_id, name, service, blob) VALUES ($1,$2,$3,$4,$5)`),
			id, "default", "name-"+id, "svc", []byte("x")); err != nil {
			t.Fatalf("seed credential %s: %v", id, err)
		}
	}
}

func TestGrantSecretAndHas(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	seedGrantTargets(t, j, []string{"wf_a"}, []string{"cred_a", "cred_b"})

	if err := j.GrantSecret(ctx, "wf_a", "cred_a", "tester", "demo"); err != nil {
		t.Fatal(err)
	}
	// Re-grant is idempotent.
	if err := j.GrantSecret(ctx, "wf_a", "cred_a", "tester2", "demo2"); err != nil {
		t.Fatal(err)
	}

	ok, err := j.HasGrant(ctx, "wf_a", "cred_a")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected grant present")
	}

	// A different (workflow, credential) pair returns false (and the
	// table is no longer empty so ErrACLEmpty doesn't apply).
	ok, err = j.HasGrant(ctx, "wf_a", "cred_b")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected (wf_a, cred_b) to be missing")
	}
}

func TestRevokeSecret(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	seedGrantTargets(t, j, []string{"wf_a"}, []string{"cred_a"})

	if err := j.GrantSecret(ctx, "wf_a", "cred_a", "tester", ""); err != nil {
		t.Fatal(err)
	}
	if err := j.RevokeSecret(ctx, "wf_a", "cred_a"); err != nil {
		t.Fatal(err)
	}
	if err := j.RevokeSecret(ctx, "wf_a", "cred_a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound on second revoke", err)
	}
}

// TestGrantAndRevokeRequireExistingTargets pins the phantom-grant fix. A live
// stdio MCP server accepted reactor_grant_secret with a workflow id that does
// not exist and returned ok:true, because the tenant guard was skipped whenever
// either row was absent and no calling surface checked existence.
func TestGrantAndRevokeRequireExistingTargets(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	seedGrantTargets(t, j, []string{"wf_real"}, []string{"cred_real"})

	if err := j.GrantSecret(ctx, "wf_TOTALLY_MADE_UP", "cred_real", "mcp", ""); !errors.Is(err, ErrGrantTarget) {
		t.Fatalf("grant to a nonexistent workflow: got %v, want ErrGrantTarget", err)
	}
	if err := j.GrantSecret(ctx, "wf_real", "cred_TOTALLY_MADE_UP", "mcp", ""); !errors.Is(err, ErrGrantTarget) {
		t.Fatalf("grant of a nonexistent credential: got %v, want ErrGrantTarget", err)
	}
	if err := j.RevokeSecret(ctx, "wf_TOTALLY_MADE_UP", "cred_real"); !errors.Is(err, ErrGrantTarget) {
		t.Fatalf("revoke naming a nonexistent workflow: got %v, want ErrGrantTarget", err)
	}
	// No phantom rows were written by any of the above.
	all, err := j.ListGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("rejected grants must not persist, got %d rows", len(all))
	}
}

// TestRevokeSecretRefusesCrossTenant mirrors TestGrantSecretRefusesCrossTenant.
// The 2026-07-07 pass added the cross-tenant guard to grant and not to revoke,
// leaving a cross-tenant denial of service on all three surfaces.
func TestRevokeSecretRefusesCrossTenant(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := j.db.ExecContext(ctx, j.bind(q), args...); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	mustExec(`INSERT INTO workflows (id, tenant_id, slug, code_hash, sdk_version, dag_json) VALUES ($1,$2,$3,$4,$5,$6)`,
		"wf_a", "tenant-a", "wf-a", "h", "0.1.0", "{}")
	mustExec(`INSERT INTO credentials (id, tenant_id, name, service, blob) VALUES ($1,$2,$3,$4,$5)`,
		"cred_b", "tenant-b", "cb", "svc", []byte("x"))

	if err := j.RevokeSecret(ctx, "wf_a", "cred_b"); err == nil {
		t.Fatal("cross-tenant revoke should be refused")
	}
}

func TestListGrantsByWorkflow(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	seedGrantTargets(t, j, []string{"wf_a", "wf_b"}, []string{"cred_1", "cred_2"})
	for _, p := range [][2]string{{"wf_a", "cred_1"}, {"wf_a", "cred_2"}, {"wf_b", "cred_1"}} {
		if err := j.GrantSecret(ctx, p[0], p[1], "tester", ""); err != nil {
			t.Fatal(err)
		}
	}
	all, err := j.ListGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("ListGrants got %d, want 3", len(all))
	}

	scoped, err := j.ListGrantsForWorkflow(ctx, "wf_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 {
		t.Fatalf("ListGrantsForWorkflow(wf_a) got %d, want 2", len(scoped))
	}
}
