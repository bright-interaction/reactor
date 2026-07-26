package journal

import (
	"context"
	"errors"
	"testing"
)

func TestCreateAndFindWebhookTrigger(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	tokenID, err := NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	id, err := j.CreateWebhookTrigger(ctx, "wf_1", tokenID, "cred_abc", "stripe", []byte(`{"event":"invoice.paid"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := j.FindWebhookByToken(ctx, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.WorkflowID != "wf_1" || got.SecretID != "cred_abc" || got.Provider != "stripe" {
		t.Fatalf("got %+v", got)
	}
	if string(got.Config) != `{"event":"invoice.paid"}` {
		t.Fatalf("config = %s", got.Config)
	}
}

func TestFindWebhookMissing(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	if _, err := j.FindWebhookByToken(context.Background(), "whk_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestRecordWebhookDeliveryDeduplicates(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	first, err := j.RecordWebhookDelivery(ctx, "trg_a", "stripe", "evt_123")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first record should report new")
	}
	second, err := j.RecordWebhookDelivery(ctx, "trg_a", "stripe", "evt_123")
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("duplicate record should report not-new")
	}
	// Different provider with same delivery_id is its own row.
	third, err := j.RecordWebhookDelivery(ctx, "trg_a", "github", "evt_123")
	if err != nil {
		t.Fatal(err)
	}
	if !third {
		t.Fatal("different provider should record as new")
	}
}

// TestWebhookDedupIsPerTrigger pins a defect that was live without any tenancy
// involved. The dedup key was (provider, delivery_id) alone, a namespace shared
// by every trigger in the install, and for the generic provider delivery_id
// comes verbatim from the caller's X-Webhook-Delivery header.
//
// So two workflows subscribed to the same GitHub org event received one
// delivery id fanned out to both URLs, the first to arrive claimed it, and the
// second workflow was answered 200 {"deduped":true} and NEVER RAN. Across
// tenants it is a deliberate denial of delivery: post to your own trigger
// carrying the victim's delivery id and their automation stops while their
// upstream keeps seeing success.
func TestWebhookDedupIsPerTrigger(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	const provider, delivery = "github", "evt_shared"

	mine, err := j.RecordWebhookDelivery(ctx, "trg_mine", provider, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if !mine {
		t.Fatal("first trigger should record as new")
	}

	// The SAME delivery id arriving at a DIFFERENT trigger must still dispatch.
	theirs, err := j.RecordWebhookDelivery(ctx, "trg_theirs", provider, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if !theirs {
		t.Fatal("another trigger's delivery was suppressed: one trigger can silence another's webhooks")
	}

	// Dedup still works WITHIN a trigger, which is the actual purpose.
	repeat, err := j.RecordWebhookDelivery(ctx, "trg_mine", provider, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if repeat {
		t.Fatal("a genuine replay to the same trigger should be deduped")
	}

	// Rollback is scoped too: releasing one trigger's claim must not release
	// another's, or a failed dispatch would re-open a neighbour's replay window.
	if err := j.DeleteWebhookDelivery(ctx, "trg_mine", provider, delivery); err != nil {
		t.Fatal(err)
	}
	again, err := j.RecordWebhookDelivery(ctx, "trg_mine", provider, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if !again {
		t.Fatal("after rollback the sender's retry should be treated as fresh")
	}
	stillClaimed, err := j.RecordWebhookDelivery(ctx, "trg_theirs", provider, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if stillClaimed {
		t.Fatal("rolling back one trigger's claim also released another's")
	}
}

func TestNewTokenIDIsUnique(t *testing.T) {
	t.Parallel()
	a, _ := NewTokenID()
	b, _ := NewTokenID()
	if a == b || a == "" || b == "" {
		t.Fatalf("got %q and %q", a, b)
	}
	if len(a) != len("whk_")+32 {
		t.Fatalf("unexpected length: %s", a)
	}
}

func TestMarkTriggerFiredAndError(t *testing.T) {
	t.Parallel()
	j, cleanup := newTestJournal(t)
	defer cleanup()
	ctx := context.Background()

	tok, _ := NewTokenID()
	id, _ := j.CreateWebhookTrigger(ctx, "wf_1", tok, "cred_x", "generic", nil)

	if err := j.MarkTriggerError(ctx, id, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkTriggerFired(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, _ := j.FindWebhookByToken(ctx, tok)
	if got.LastError != "" {
		t.Fatalf("error not cleared: %q", got.LastError)
	}
	if got.LastFiredAt == nil {
		t.Fatal("last_fired_at not set")
	}
}
