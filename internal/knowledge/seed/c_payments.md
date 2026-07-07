---
id: c_payments
topic: connectors
title: Take payments with Stripe or Mollie
created_by: seed
sources: [https://docs.stripe.com/api/checkout/sessions/create, https://docs.mollie.com/reference/create-payment]
tags: [payments, stripe, mollie, connectors, side-effects, idempotency]
gold: true
citation_count: 0
---

# Take payments with Stripe or Mollie

Use `sdk/stripe` or `sdk/mollie` instead of hand-building payment API calls.
The API key is a plain (non-OAuth) vault credential; fetch it at run time and
never log it. Creating a charge, checkout, or refund is a side effect, so wrap
it in a Step with a deterministic `IdempotencyKey` (see [[i_step-keys]]) and
pass that key through so a replay never double-charges.

## Stripe: hosted checkout

```go
import "github.com/brightinteraction/reactor/sdk/stripe"

out, err := reactor.Step(flow, ctx, "checkout", reactor.StepOpts{
    IdempotencyKey: "checkout:" + in.OrderID,
    Timeout:        30 * time.Second,
}, func(ctx context.Context) (stripe.CheckoutSession, error) {
    c := &stripe.Client{Key: string(vault.MustGet("stripe-key").Reveal())}
    return c.CreateCheckoutSession(ctx, stripe.CheckoutParams{
        Mode:       "payment",
        LineItems:  []stripe.LineItem{{Price: in.PriceID, Quantity: 1}},
        SuccessURL: in.SuccessURL,
        CancelURL:  in.CancelURL,
    }, "checkout:"+in.OrderID)
})
// redirect or email out.URL so the customer can pay
```

## Mollie: create a payment (EU)

```go
import "github.com/brightinteraction/reactor/sdk/mollie"

c := &mollie.Client{Key: string(vault.MustGet("mollie-key").Reveal())}
pay, err := c.CreatePayment(ctx, mollie.PaymentParams{
    Amount:      mollie.Amount{Currency: "EUR", Value: "10.00"}, // value is a string
    Description: "Order " + in.OrderID,
    RedirectURL: in.ReturnURL,
    WebhookURL:  in.WebhookURL, // optional: Mollie POSTs status changes here
})
// send the customer to pay.CheckoutURL; later poll c.GetPayment(ctx, pay.ID).Status
```

## Rules

- Key id is a plain name (`stripe-key`, `mollie-key`), not `oauth:`.
- Stripe amounts are integer minor units (cents). Mollie `Amount.Value` is a
  decimal string like "10.00".
- Always set the Step `IdempotencyKey`; for Stripe pass it to the call too so
  it rides as the `Idempotency-Key` header.
- A Mollie webhook (or a Stripe webhook trigger with HMAC verify) is the right
  way to react to "payment paid", rather than polling in a tight loop.
