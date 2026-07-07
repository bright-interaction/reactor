// stripe-checkout turns a webhook into a Stripe hosted checkout link. It reads
// an order from the payload, creates a Checkout Session with the Stripe key
// from the vault, and returns the pay URL (which a real workflow would email or
// redirect to). Charging is a side effect, so the step carries an idempotency
// key that also rides as Stripe's Idempotency-Key header: a replay of the same
// order never creates a second session.
//
// Build:    reactor workflow build --src examples/stripe-checkout --slug stripe-checkout
// Register: reactor workflow register --db ... --slug stripe-checkout
// Trigger:  POST /webhook/<token> with
//           {"order_id":"o_1","price_id":"price_123","success_url":"https://x/ok","cancel_url":"https://x/no"}
package main

import (
	"context"
	"errors"
	"time"

	"github.com/brightinteraction/reactor/sdk"
	"github.com/brightinteraction/reactor/sdk/runtime"
	"github.com/brightinteraction/reactor/sdk/stripe"
	"github.com/brightinteraction/reactor/sdk/vault"
)

//nolint:gochecknoglobals // SDK contract: package-level declarations.
var (
	Workflow = reactor.Workflow{Slug: "stripe-checkout", Version: "0.1.0"}
	Trigger  = reactor.WebhookTrigger{Path: "/webhook/stripe-checkout", Provider: "generic"}
)

// Payload is the webhook body describing the order to charge for.
type Payload struct {
	OrderID    string `json:"order_id"`
	PriceID    string `json:"price_id"`
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
}

// Checkout is the step output: the hosted pay URL, journaled so a replay
// returns it without creating a second session.
type Checkout struct {
	SessionID string `json:"session_id"`
	PayURL    string `json:"pay_url"`
}

func Run(ctx context.Context, flow reactor.Flow, in Payload) error {
	log := flow.Logger()
	log.Info("stripe-checkout: starting", "order", in.OrderID)
	if in.OrderID == "" || in.PriceID == "" {
		return reactor.Permanent(errors.New("stripe-checkout: order_id and price_id are required"))
	}

	_, err := reactor.Step(flow, ctx, "checkout", reactor.StepOpts{
		IdempotencyKey: "checkout:" + in.OrderID,
		Timeout:        30 * time.Second,
	}, func(ctx context.Context) (Checkout, error) {
		c := &stripe.Client{Key: string(vault.MustGet("stripe-key").Reveal())}
		sess, err := c.CreateCheckoutSession(ctx, stripe.CheckoutParams{
			Mode:       "payment",
			LineItems:  []stripe.LineItem{{Price: in.PriceID, Quantity: 1}},
			SuccessURL: in.SuccessURL,
			CancelURL:  in.CancelURL,
		}, "checkout:"+in.OrderID)
		if err != nil {
			return Checkout{}, err
		}
		return Checkout{SessionID: sess.ID, PayURL: sess.URL}, nil
	})
	return err
}

func main() {
	runtime.Serve(Workflow, Trigger, Run)
}
