// Package notifier delivers run-status alerts to configured channels.
//
// One Notifier instance owns a small set of senders (slack_webhook,
// generic_webhook, email_smtp). The dispatcher's OnTerminal callback
// invokes Notify with the (run, status, error_text) triple; the
// notifier looks up routed channels via journal.ChannelsForRunTerminal
// and fires each in a bounded-concurrency fan-out.
//
// Failure modes are intentionally non-blocking: a Slack outage must
// not stall the dispatcher's terminal path. Each sender has a
// per-attempt timeout (5s default) and errors are logged at WARN
// without bubbling. Operators expecting "guaranteed delivery" should
// route to a queue-backed webhook (e.g. an internal POST endpoint
// that lands the alert in a journal of its own).
package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// Event is everything a sender needs to render an alert. Snake_case
// JSON tags so generic-webhook receivers can route without re-mapping.
type Event struct {
	RunID        string    `json:"run_id"`
	WorkflowSlug string    `json:"workflow_slug"`
	WorkflowID   string    `json:"workflow_id"`
	Status       string    `json:"status"` // "succeeded" | "failed" | "failed_dlq"
	TriggerKind  string    `json:"trigger_kind"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	ErrorText    string    `json:"error_text,omitempty"` // last step's error, empty on success
	DashboardURL string    `json:"dashboard_url,omitempty"`
}

// Sender is the per-kind delivery shape. Implementations should be
// stateless or own internal pool state; the Notifier instantiates
// one per channel kind, not one per channel.
type Sender interface {
	Send(ctx context.Context, cfg json.RawMessage, ev Event) error
	Kind() string
}

// ChannelLookup is the read surface the notifier needs from the
// journal. Defined as an interface so tests can stub without spinning
// up a real DB.
type ChannelLookup interface {
	ChannelsForRunTerminal(ctx context.Context, workflowID, status string) ([]journal.NotificationChannel, error)
	GetNotificationChannel(ctx context.Context, id string) (journal.NotificationChannel, error)
}

// Notifier dispatches Events to routed channels. Construct one per
// daemon via New + register senders via WithSender.
type Notifier struct {
	lookup       ChannelLookup
	senders      map[string]Sender
	log          *slog.Logger
	dashboardURL string
	timeout      time.Duration

	// resolveSecret resolves a vault credential id to its plaintext value.
	// Optional; nil means channel configs are used verbatim. Wired by the
	// daemon to the vault so a channel can reference a credential by id
	// (e.g. an SMTP password or a webhook auth header) instead of storing
	// the secret in plaintext in config_json.
	resolveSecret func(ctx context.Context, credentialID string) (string, error)
}

// WithSecretResolver wires the vault lookup used to resolve channel
// credential references at send time. Returns the Notifier for chaining.
func (n *Notifier) WithSecretResolver(f func(ctx context.Context, credentialID string) (string, error)) *Notifier {
	n.resolveSecret = f
	return n
}

// New builds a Notifier with the default 5s per-send timeout. Senders
// must be registered with WithSender before Notify will deliver.
func New(lookup ChannelLookup, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		lookup:  lookup,
		senders: map[string]Sender{},
		log:     log,
		timeout: 5 * time.Second,
	}
}

// WithSender registers a sender keyed by its Kind(). Subsequent
// registrations under the same kind replace the previous sender.
func (n *Notifier) WithSender(s Sender) *Notifier {
	n.senders[s.Kind()] = s
	return n
}

// WithDashboardURL sets the public base URL used to render "view this
// run" links in the alert body. Empty (the default) means senders
// omit the link.
func (n *Notifier) WithDashboardURL(u string) *Notifier {
	n.dashboardURL = u
	return n
}

// WithTimeout overrides the default 5s per-send timeout.
func (n *Notifier) WithTimeout(d time.Duration) *Notifier {
	if d > 0 {
		n.timeout = d
	}
	return n
}

// Notify dispatches the event to every channel routed for the
// (workflow_id, status) pair. Per-sender failures are logged at
// WARN; the function never returns an error so the dispatcher's
// terminal path stays unblocked.
//
// dashboardURL is the public base URL ("https://reactor.acme.com"); the
// sender appends "/runs/<id>" to build a clickable link.
func (n *Notifier) Notify(ctx context.Context, ev Event) {
	channels, err := n.lookup.ChannelsForRunTerminal(ctx, ev.WorkflowID, ev.Status)
	if err != nil {
		n.log.Warn("notifier: channel lookup failed", "err", err, "workflow_id", ev.WorkflowID, "status", ev.Status)
		return
	}
	if len(channels) == 0 {
		return
	}
	if ev.DashboardURL == "" && n.dashboardURL != "" {
		ev.DashboardURL = n.dashboardURL + "/runs/" + ev.RunID
	}
	var wg sync.WaitGroup
	for _, ch := range channels {
		ch := ch
		sender, ok := n.senders[ch.Kind]
		if !ok {
			n.log.Warn("notifier: no sender registered for kind",
				"kind", ch.Kind, "channel", ch.Name)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A panicking sender must not take down the daemon mid-flight;
			// isolate it to this one channel send.
			defer func() {
				if rec := recover(); rec != nil {
					n.log.Error("notifier: sender panicked",
						"kind", ch.Kind, "channel", ch.Name, "panic", rec)
				}
			}()
			n.dispatch(ctx, ch, sender, ev)
		}()
	}
	wg.Wait()
}

// TestChannel fires a synthetic alert through one channel id so the
// operator's "Send test" button can confirm credentials + connectivity
// without waiting for a real run to fail.
func (n *Notifier) TestChannel(ctx context.Context, channelID string) error {
	ch, err := n.lookup.GetNotificationChannel(ctx, channelID)
	if err != nil {
		return fmt.Errorf("notifier: lookup channel: %w", err)
	}
	sender, ok := n.senders[ch.Kind]
	if !ok {
		return fmt.Errorf("notifier: no sender registered for kind %q", ch.Kind)
	}
	ev := Event{
		RunID:        "test_" + fmtNow(),
		WorkflowSlug: "(test channel)",
		WorkflowID:   "test",
		Status:       "test",
		TriggerKind:  "test",
		StartedAt:    time.Now().UTC(),
		FinishedAt:   time.Now().UTC(),
		ErrorText:    "Test message from Reactor. Channel is wired correctly.",
		DashboardURL: n.dashboardURL,
	}
	tctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	return sender.Send(tctx, ch.ConfigJSON, ev)
}

func (n *Notifier) dispatch(ctx context.Context, ch journal.NotificationChannel, sender Sender, ev Event) {
	tctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	cfg := ch.ConfigJSON
	if n.resolveSecret != nil {
		resolved, err := resolveChannelSecrets(tctx, ch.Kind, ch.ConfigJSON, n.resolveSecret)
		if err != nil {
			// Fail closed: a channel that asked for a vault secret but
			// couldn't get it must NOT silently fall back to sending with
			// a blank credential (that would leak alerts unauthenticated
			// or fail confusingly). Skip this send.
			n.log.Warn("notifier: resolve channel secret failed; skipping send",
				"channel", ch.Name, "kind", ch.Kind, "err", err)
			return
		}
		cfg = resolved
	}
	if err := sender.Send(tctx, cfg, ev); err != nil {
		n.log.Warn("notifier: send failed",
			"channel", ch.Name, "kind", ch.Kind, "run_id", ev.RunID, "err", err)
		return
	}
	n.log.Info("notifier: alert sent",
		"channel", ch.Name, "kind", ch.Kind, "run_id", ev.RunID, "status", ev.Status)
}

func fmtNow() string {
	return time.Now().UTC().Format("20060102T150405Z")
}
