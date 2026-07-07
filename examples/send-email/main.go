// send-email is the smallest workflow that sends through a connected account.
// It takes a webhook payload naming a Google connection (connected once on the
// Connections page) plus the message, and sends it via the Gmail API using a
// token the host resolves from "oauth:<connection-id>" at run time. The token
// never appears in this source, only the connection id does.
//
// Build:    reactor workflow build --src examples/send-email --slug send-email
// Register: reactor workflow register --db ... --slug send-email
// Trigger:  POST /webhook/<token> with
//           {"connection_id":"conn_x","from":"me@x.com","to":"c@y.com",
//            "subject":"Hi","body":"hello"}
package main

import (
	"context"
	"errors"
	"time"

	"github.com/brightinteraction/reactor/sdk"
	"github.com/brightinteraction/reactor/sdk/email"
	"github.com/brightinteraction/reactor/sdk/runtime"
	"github.com/brightinteraction/reactor/sdk/vault"
)

//nolint:gochecknoglobals // SDK contract: package-level declarations.
var (
	Workflow = reactor.Workflow{Slug: "send-email", Version: "0.1.0"}
	Trigger  = reactor.WebhookTrigger{Path: "/webhook/send-email", Provider: "generic"}
)

// Payload is the webhook body. ConnectionID points at a Google account
// connected on the Connections page; the host resolves "oauth:<id>" to a live,
// auto-refreshed access token when the step runs.
type Payload struct {
	ConnectionID string `json:"connection_id"`
	From         string `json:"from"`
	To           string `json:"to"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
}

// Sent is the step output: the Gmail message id, journaled so a replay returns
// it without sending again.
type Sent struct {
	MessageID string `json:"message_id"`
}

func Run(ctx context.Context, flow reactor.Flow, in Payload) error {
	log := flow.Logger()
	log.Info("send-email: starting", "to", in.To)
	if in.ConnectionID == "" || in.To == "" {
		return reactor.Permanent(errors.New("send-email: connection_id and to are required"))
	}

	_, err := reactor.Step(flow, ctx, "send", reactor.StepOpts{
		// Deterministic key: a replay of the same message is a no-op.
		IdempotencyKey: in.ConnectionID + ":" + in.To + ":" + in.Subject,
		Timeout:        30 * time.Second,
	}, func(ctx context.Context) (Sent, error) {
		tok := vault.MustGet("oauth:" + in.ConnectionID)
		id, err := email.SendGmail(ctx, string(tok.Reveal()), email.Message{
			From:    in.From,
			To:      []string{in.To},
			Subject: in.Subject,
			Text:    in.Body,
		})
		if err != nil {
			return Sent{}, err
		}
		return Sent{MessageID: id}, nil
	})
	return err
}

func main() {
	runtime.Serve(Workflow, Trigger, Run)
}
