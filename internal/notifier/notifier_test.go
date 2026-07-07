package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brightinteraction/reactor/internal/runtime/journal"
)

// fakeLookup implements ChannelLookup so the notifier tests don't
// require a real journal.
type fakeLookup struct {
	channels  []journal.NotificationChannel
	byID      map[string]journal.NotificationChannel
	lookupErr error
}

func (f *fakeLookup) ChannelsForRunTerminal(_ context.Context, workflowID, status string) ([]journal.NotificationChannel, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	return f.channels, nil
}

func (f *fakeLookup) GetNotificationChannel(_ context.Context, id string) (journal.NotificationChannel, error) {
	ch, ok := f.byID[id]
	if !ok {
		return journal.NotificationChannel{}, errors.New("not found")
	}
	return ch, nil
}

// countingSender records call counts + the last event so dispatch
// behaviour can be asserted without a real HTTP server.
type countingSender struct {
	kind  string
	calls atomic.Int32
	last  Event
	fail  error
}

func (c *countingSender) Kind() string { return c.kind }
func (c *countingSender) Send(_ context.Context, _ json.RawMessage, ev Event) error {
	c.calls.Add(1)
	c.last = ev
	return c.fail
}

func TestNotifyFiresEverySender(t *testing.T) {
	t.Parallel()
	chSlack := journal.NotificationChannel{ID: "c1", Name: "ops", Kind: "slack_webhook", ConfigJSON: []byte(`{"url":"x"}`)}
	chHook := journal.NotificationChannel{ID: "c2", Name: "ops2", Kind: "generic_webhook", ConfigJSON: []byte(`{"url":"x"}`)}
	lookup := &fakeLookup{channels: []journal.NotificationChannel{chSlack, chHook}}
	slackS := &countingSender{kind: "slack_webhook"}
	hookS := &countingSender{kind: "generic_webhook"}

	n := New(lookup, nil).WithSender(slackS).WithSender(hookS).WithDashboardURL("https://reactor.example.com")
	n.Notify(context.Background(), Event{RunID: "r1", WorkflowID: "wf_1", Status: "failed"})

	if got := slackS.calls.Load(); got != 1 {
		t.Fatalf("slack calls = %d, want 1", got)
	}
	if got := hookS.calls.Load(); got != 1 {
		t.Fatalf("hook calls = %d, want 1", got)
	}
	if slackS.last.DashboardURL != "https://reactor.example.com/runs/r1" {
		t.Fatalf("DashboardURL = %q", slackS.last.DashboardURL)
	}
}

func TestNotifySwallowsSenderErrors(t *testing.T) {
	t.Parallel()
	ch := journal.NotificationChannel{ID: "c1", Kind: "generic_webhook", ConfigJSON: []byte(`{"url":"x"}`)}
	lookup := &fakeLookup{channels: []journal.NotificationChannel{ch}}
	bad := &countingSender{kind: "generic_webhook", fail: errors.New("boom")}
	n := New(lookup, nil).WithSender(bad)
	// Must not panic + must not return an error (signature is void).
	n.Notify(context.Background(), Event{RunID: "r1", WorkflowID: "wf_1", Status: "failed"})
	if got := bad.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestNotifySkipsUnregisteredKind(t *testing.T) {
	t.Parallel()
	ch := journal.NotificationChannel{ID: "c1", Kind: "mystery_kind", ConfigJSON: []byte(`{}`)}
	lookup := &fakeLookup{channels: []journal.NotificationChannel{ch}}
	n := New(lookup, nil) // no senders registered
	n.Notify(context.Background(), Event{RunID: "r1", WorkflowID: "wf_1", Status: "failed"})
	// No panic, no crash; the warn log fires (untested here).
}

func TestTestChannelDeliversToSender(t *testing.T) {
	t.Parallel()
	ch := journal.NotificationChannel{ID: "c1", Kind: "generic_webhook", ConfigJSON: []byte(`{"url":"x"}`)}
	lookup := &fakeLookup{byID: map[string]journal.NotificationChannel{"c1": ch}}
	s := &countingSender{kind: "generic_webhook"}
	n := New(lookup, nil).WithSender(s)
	if err := n.TestChannel(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if got := s.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if s.last.Status != "test" {
		t.Fatalf("event status = %q, want test", s.last.Status)
	}
}

func TestSlackSenderPostsBlockKit(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	// ssrfSafeClient(true) so the loopback httptest server is reachable (the
	// production NewSlackSender blocks private IPs).
	sender := &SlackSender{Client: ssrfSafeClient(true)}
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL})
	ev := Event{RunID: "r1", WorkflowSlug: "demo", Status: "failed", ErrorText: "step blew up"}
	if err := sender.Send(context.Background(), cfg, ev); err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/json" {
		t.Fatalf("content type = %q", gotCT)
	}
	if !strings.Contains(string(gotBody), `"blocks"`) || !strings.Contains(string(gotBody), `demo`) {
		t.Fatalf("missing block payload: %s", gotBody)
	}
}

func TestSlackSenderSurfacesNon2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte("invalid_token"))
	}))
	defer srv.Close()
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL})
	err := (&SlackSender{Client: ssrfSafeClient(true)}).Send(context.Background(), cfg, Event{Status: "failed"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want 403 surfaced", err)
	}
}

func TestWebhookSenderPostsJSONWithHeaders(t *testing.T) {
	t.Parallel()
	var gotBody string
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotHeader = r.Header.Get("X-Auth")
	}))
	defer srv.Close()
	cfg, _ := json.Marshal(map[string]any{
		"url":     srv.URL,
		"headers": map[string]string{"X-Auth": "secret"},
	})
	ev := Event{RunID: "r1", WorkflowSlug: "wf", Status: "failed"}
	if err := (&WebhookSender{Client: ssrfSafeClient(true)}).Send(context.Background(), cfg, ev); err != nil {
		t.Fatal(err)
	}
	if gotHeader != "secret" {
		t.Fatalf("auth header = %q", gotHeader)
	}
	if !strings.Contains(gotBody, `"run_id":"r1"`) {
		t.Fatalf("body missing run_id: %s", gotBody)
	}
}

func TestSlackSenderRejectsEmptyURL(t *testing.T) {
	t.Parallel()
	err := NewSlackSender().Send(context.Background(), []byte(`{}`), Event{Status: "failed"})
	if err == nil || !strings.Contains(err.Error(), "missing url") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotifyHonoursPerSendTimeout(t *testing.T) {
	t.Parallel()
	// Slow handler (1s); notifier timeout 50ms.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	ch := journal.NotificationChannel{ID: "c1", Kind: "generic_webhook", ConfigJSON: mustJSON(map[string]string{"url": srv.URL})}
	lookup := &fakeLookup{channels: []journal.NotificationChannel{ch}}
	n := New(lookup, nil).WithSender((&WebhookSender{Client: ssrfSafeClient(true)})).WithTimeout(50 * time.Millisecond)
	start := time.Now()
	n.Notify(context.Background(), Event{RunID: "r1", WorkflowID: "wf_1", Status: "failed"})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("notify did not honour timeout; took %s", elapsed)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
