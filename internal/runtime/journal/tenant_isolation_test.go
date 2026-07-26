package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestTenantIsolationIsRealNow is the test that could not be written before.
//
// CreateWorkflow's INSERT omitted tenant_id, so every workflow took the schema
// default and no supported code path could put one in another tenant. The
// isolation machinery built on top (viewerScope, WorkflowTenant, the
// cross-tenant grant guard, per-tenant plans and quotas) was therefore
// unfalsifiable: the guard compared two values that were always equal, and no
// test could construct the two-tenant state that would expose it.
func TestTenantIsolationIsRealNow(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	if err := j.CreateWorkflowInTenant(ctx, "wf_a", "shared-slug", "h", "0.1.0", json.RawMessage(`{}`), "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateWorkflowInTenant(ctx, "wf_b", "shared-slug", "h", "0.1.0", json.RawMessage(`{}`), "tenant-b"); err != nil {
		// Two tenants owning the same slug is exactly what UNIQUE (tenant_id,
		// slug) permits, and what the unscoped resolver got wrong.
		t.Fatalf("per-tenant slug uniqueness should allow this: %v", err)
	}

	// Each tenant resolves ITS OWN workflow, not whichever was created last.
	gotA, err := j.WorkflowIDBySlugInTenant(ctx, "shared-slug", "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if gotA != "wf_a" {
		t.Fatalf("tenant-a resolved %q, want wf_a: the resolver crossed the tenant boundary", gotA)
	}
	gotB, err := j.WorkflowIDBySlugInTenant(ctx, "shared-slug", "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	if gotB != "wf_b" {
		t.Fatalf("tenant-b resolved %q, want wf_b", gotB)
	}

	// A tenant that owns nothing gets nothing rather than someone else's row.
	if _, err := j.WorkflowIDBySlugInTenant(ctx, "shared-slug", "tenant-c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated tenant got %v, want ErrNotFound", err)
	}

	// WorkflowTenant reports the real owner, which is what the read-path guards
	// (dashboard.go, workflow_lifecycle.go) compare against viewerScope.
	owner, err := j.WorkflowTenant(ctx, "wf_b")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "tenant-b" {
		t.Fatalf("WorkflowTenant = %q, want tenant-b", owner)
	}

	// And listing is scoped.
	listA, err := j.ListWorkflowsByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].ID != "wf_a" {
		t.Fatalf("tenant-a listing = %+v, want only wf_a", listA)
	}
}

// TestCrossTenantGrantGuardCanActuallyFire is the payoff. The guard shipped in
// the 2026-07-07 pass as "the data-layer half of the cross-tenant
// secret-disclosure fix", but with every workflow and credential pinned to one
// tenant its comparison was permanently false. It could not fire, and no test
// could show that it did.
func TestCrossTenantGrantGuardCanActuallyFire(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	if err := j.CreateWorkflowInTenant(ctx, "wf_a", "wf-a", "h", "0.1.0", json.RawMessage(`{}`), "tenant-a"); err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := j.db.ExecContext(ctx, j.bind(q), args...); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	mustExec(`INSERT INTO credentials (id, tenant_id, name, service, blob) VALUES ($1,$2,$3,$4,$5)`,
		"cred_a", "tenant-a", "ca", "svc", []byte("x"))
	mustExec(`INSERT INTO credentials (id, tenant_id, name, service, blob) VALUES ($1,$2,$3,$4,$5)`,
		"cred_b", "tenant-b", "cb", "svc", []byte("x"))

	if err := j.GrantSecret(ctx, "wf_a", "cred_b", "tester", ""); err == nil {
		t.Fatal("granting tenant-a's workflow access to tenant-b's credential must be refused")
	}
	if err := j.GrantSecret(ctx, "wf_a", "cred_a", "tester", ""); err != nil {
		t.Fatalf("same-tenant grant must still work: %v", err)
	}
	// Revoke is guarded on the same axis.
	if err := j.RevokeSecret(ctx, "wf_a", "cred_b"); err == nil {
		t.Fatal("cross-tenant revoke must be refused")
	}
}

// TestCreateWorkflowDefaultsToDefaultTenant keeps the unscoped entry point
// behaving exactly as before for the CLI, MCP and codegen callers that have no
// tenant context.
func TestCreateWorkflowDefaultsToDefaultTenant(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	if err := j.CreateWorkflow(ctx, "wf_x", "unscoped", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	owner, err := j.WorkflowTenant(ctx, "wf_x")
	if err != nil {
		t.Fatal(err)
	}
	if owner != DefaultTenant {
		t.Fatalf("unscoped CreateWorkflow put the row in %q, want %q", owner, DefaultTenant)
	}
	// An empty tenant argument means the same thing, not an empty string owner.
	if err := j.CreateWorkflowInTenant(ctx, "wf_y", "explicit-empty", "h", "0.1.0", json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	owner, _ = j.WorkflowTenant(ctx, "wf_y")
	if owner != DefaultTenant {
		t.Fatalf("empty tenant argument produced %q, want %q", owner, DefaultTenant)
	}
}
