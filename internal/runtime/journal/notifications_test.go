package journal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCreateNotificationChannelRejectsBadKind(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	_, err := j.CreateNotificationChannel(context.Background(), "ops", "carrier-pigeon", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("bad kind should have been rejected")
	}
}

func TestCreateNotificationChannelRoundTrip(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	id, err := j.CreateNotificationChannel(ctx, "ops-slack", "slack_webhook", json.RawMessage(`{"url":"https://hooks.slack.com/services/X/Y/Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := j.GetNotificationChannel(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name != "ops-slack" || ch.Kind != "slack_webhook" {
		t.Fatalf("got %+v", ch)
	}
	list, err := j.ListNotificationChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %+v", list)
	}
}

func TestNotificationRouteUpsertNormalisesStatuses(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	id, err := j.CreateNotificationChannel(ctx, "c1", "generic_webhook", json.RawMessage(`{"url":"https://x.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Statuses with weird whitespace + duplicates + uppercase.
	if err := j.AddNotificationRoute(ctx, "wf_1", id, " Failed ,FAILED_DLQ, failed "); err != nil {
		t.Fatal(err)
	}
	routes, err := j.ListNotificationRoutesForWorkflow(ctx, "wf_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %+v", routes)
	}
	want := "failed,failed_dlq"
	if routes[0].OnStatuses != want {
		t.Fatalf("on_statuses = %q, want %q", routes[0].OnStatuses, want)
	}
	// Re-add with a different shape -> upserts.
	if err := j.AddNotificationRoute(ctx, "wf_1", id, "succeeded"); err != nil {
		t.Fatal(err)
	}
	routes, _ = j.ListNotificationRoutesForWorkflow(ctx, "wf_1")
	if routes[0].OnStatuses != "succeeded" {
		t.Fatalf("upsert failed: %q", routes[0].OnStatuses)
	}
}

func TestChannelsForRunTerminalFilters(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	id, err := j.CreateNotificationChannel(ctx, "ops", "generic_webhook", json.RawMessage(`{"url":"https://x.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.AddNotificationRoute(ctx, "wf_1", id, "failed,failed_dlq"); err != nil {
		t.Fatal(err)
	}

	// failed_dlq fires.
	got, err := j.ChannelsForRunTerminal(ctx, "wf_1", "failed_dlq")
	if err != nil || len(got) != 1 {
		t.Fatalf("failed_dlq: got=%+v err=%v", got, err)
	}
	// succeeded does NOT fire under the default route.
	got, err = j.ChannelsForRunTerminal(ctx, "wf_1", "succeeded")
	if err != nil || len(got) != 0 {
		t.Fatalf("succeeded: got=%+v err=%v", got, err)
	}
	// Different workflow returns nothing.
	got, _ = j.ChannelsForRunTerminal(ctx, "wf_other", "failed")
	if len(got) != 0 {
		t.Fatalf("other workflow: %+v", got)
	}
}

func TestDeleteChannelRefusesInUse(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	id, _ := j.CreateNotificationChannel(ctx, "ops", "generic_webhook", json.RawMessage(`{"url":"https://x.com"}`))
	_ = j.AddNotificationRoute(ctx, "wf_1", id, "failed")
	if err := j.DeleteNotificationChannel(ctx, id); !errors.Is(err, ErrChannelInUse) {
		t.Fatalf("err = %v, want ErrChannelInUse", err)
	}
	// Detach + delete succeeds.
	_ = j.DeleteNotificationRoute(ctx, "wf_1", id)
	if err := j.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatalf("after detach: %v", err)
	}
}

func TestDeleteWorkflowCascadesNotificationRoutes(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	id, _ := j.CreateNotificationChannel(ctx, "ops", "generic_webhook", json.RawMessage(`{"url":"https://x.com"}`))
	_ = j.AddNotificationRoute(ctx, "wf_1", id, "failed")
	// Move the seeded run terminal so DeleteWorkflow proceeds.
	if err := j.MarkRunFinished(ctx, "run_1", "succeeded"); err != nil {
		t.Fatal(err)
	}
	if err := j.DeleteWorkflow(ctx, "wf_1"); err != nil {
		t.Fatalf("delete workflow: %v", err)
	}
	routes, _ := j.ListNotificationRoutesForWorkflow(ctx, "wf_1")
	if len(routes) != 0 {
		t.Fatalf("routes survived cascade: %+v", routes)
	}
	// Channel itself stays (only the route was tied to the workflow).
	if _, err := j.GetNotificationChannel(ctx, id); err != nil {
		t.Fatalf("channel should still exist: %v", err)
	}
}

func TestAddRouteRejectsEmptyStatuses(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()
	id, _ := j.CreateNotificationChannel(ctx, "ops", "generic_webhook", json.RawMessage(`{"url":"https://x.com"}`))
	if err := j.AddNotificationRoute(ctx, "wf_1", id, "  ,, "); err == nil || !strings.Contains(err.Error(), "on_statuses") {
		t.Fatalf("empty on_statuses should have been rejected, got %v", err)
	}
}
