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

	first, err := j.RecordWebhookDelivery(ctx, "stripe", "evt_123")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first record should report new")
	}
	second, err := j.RecordWebhookDelivery(ctx, "stripe", "evt_123")
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("duplicate record should report not-new")
	}
	// Different provider with same delivery_id is its own row.
	third, err := j.RecordWebhookDelivery(ctx, "github", "evt_123")
	if err != nil {
		t.Fatal(err)
	}
	if !third {
		t.Fatal("different provider should record as new")
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
