package migrate

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
)

// TestChannelRebuildPreservesRoutes is the test the credentials migration
// needed and did not have.
//
// 0025 originally narrowed credentials' UNIQUE the same way 0027 narrows
// notification_channels': rename the parent, create it fresh, copy, drop the old
// one. On SQLite that silently destroys the CHILD rows, because RENAME rewrites
// the child's REFERENCES to point at the renamed table and the DROP then
// cascades. It cost credential_audit its entire trail, and the only reason it was
// caught was an unrelated assertion downstream.
//
// A from-scratch schema cannot catch this: there are no child rows to lose. So
// this stages the database at 0026, inserts a channel AND a route pointing at it,
// then steps to 0027 and checks the route is still there. Revert 0027 to a naive
// rename-copy-drop and this fails on the route count.
func TestChannelRebuildPreservesRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dbPath := filepath.Join(t.TempDir(), "rebuild.db")
	url := "sqlite://" + dbPath

	// Stage at 0026: tenant_id exists, name is still globally UNIQUE.
	if err := upTo(ctx, log, url, 26); err != nil {
		t.Fatalf("migrate to 0026: %v", err)
	}

	db, err := sql.Open("sqlite", sqliteDSNWithPragmas(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO workflows (id, tenant_id, slug, code_hash, sdk_version, dag_json) VALUES (?,?,?,?,?,?)`,
		"wf_1", "acme", "demo", "h", "0.1.0", `{}`)
	mustExec(`INSERT INTO notification_channels (id, tenant_id, name, kind, config_json) VALUES (?,?,?,?,?)`,
		"ch_1", "acme", "ops-slack", "slack_webhook", `{"url":"https://hooks.slack.com/x"}`)
	mustExec(`INSERT INTO workflow_notification_routes (workflow_id, channel_id, on_statuses) VALUES (?,?,?)`,
		"wf_1", "ch_1", "failed,succeeded")

	// Before: the global UNIQUE must still be in force, otherwise this test is
	// staged at the wrong revision and proves nothing.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO notification_channels (id, tenant_id, name, kind, config_json) VALUES (?,?,?,?,?)`,
		"ch_2", "globex", "ops-slack", "slack_webhook", `{"url":"https://hooks.slack.com/y"}`); err == nil {
		t.Fatal("0026 should still enforce a global UNIQUE(name); the test is staged at the wrong revision")
	}
	db.Close()

	if err := upTo(ctx, log, url, 27); err != nil {
		t.Fatalf("migrate 0026 -> 0027: %v", err)
	}

	db, err = sql.Open("sqlite", sqliteDSNWithPragmas(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The route survived, with its columns intact rather than defaulted.
	var wfID, chID, statuses string
	err = db.QueryRowContext(ctx,
		`SELECT workflow_id, channel_id, on_statuses FROM workflow_notification_routes`).
		Scan(&wfID, &chID, &statuses)
	if err == sql.ErrNoRows {
		t.Fatal("the rebuild destroyed the route: this is the credential_audit failure repeating")
	}
	if err != nil {
		t.Fatal(err)
	}
	if wfID != "wf_1" || chID != "ch_1" || statuses != "failed,succeeded" {
		t.Fatalf("route came back as %q/%q/%q, want wf_1/ch_1/failed,succeeded", wfID, chID, statuses)
	}

	// The channel survived too, tenant and all.
	var tenant, name string
	if err := db.QueryRowContext(ctx,
		`SELECT tenant_id, name FROM notification_channels WHERE id = ?`, "ch_1").Scan(&tenant, &name); err != nil {
		t.Fatalf("channel lost in rebuild: %v", err)
	}
	if tenant != "acme" || name != "ops-slack" {
		t.Fatalf("channel came back as %q/%q, want acme/ops-slack", tenant, name)
	}

	// The point of the migration: another tenant can now hold the same name.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO notification_channels (id, tenant_id, name, kind, config_json) VALUES (?,?,?,?,?)`,
		"ch_2", "globex", "ops-slack", "slack_webhook", `{"url":"https://hooks.slack.com/y"}`); err != nil {
		t.Fatalf("two tenants must be able to own the same channel name: %v", err)
	}

	// But it is still unique WITHIN a tenant, so the narrowing did not simply
	// drop the constraint.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO notification_channels (id, tenant_id, name, kind, config_json) VALUES (?,?,?,?,?)`,
		"ch_3", "acme", "ops-slack", "slack_webhook", `{"url":"https://hooks.slack.com/z"}`); err == nil {
		t.Fatal("a duplicate name within one tenant must still be refused")
	}

	// The FK is real on the rebuilt pair, not silently dropped during the
	// recreate. A route to a nonexistent channel must be refused.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_notification_routes (workflow_id, channel_id) VALUES (?,?)`,
		"wf_1", "ch_nope"); err == nil {
		t.Fatal("the recreated FK to notification_channels is not enforced")
	}

	// And the cascade still works, which is what the route table's ON DELETE
	// CASCADE exists for.
	if _, err := db.ExecContext(ctx, `DELETE FROM notification_channels WHERE id = ?`, "ch_1"); err != nil {
		t.Fatal(err)
	}
	var routes int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_notification_routes WHERE channel_id = ?`, "ch_1").Scan(&routes); err != nil {
		t.Fatal(err)
	}
	if routes != 0 {
		t.Fatalf("deleting a channel left %d orphan routes; ON DELETE CASCADE was lost in the rebuild", routes)
	}

	// The staging tables must not survive into the live schema.
	for _, tbl := range []string{"_routes_staging", "_channels_old", "_rebuild_assert"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("migration left %s behind in the schema", tbl)
		}
	}
}
