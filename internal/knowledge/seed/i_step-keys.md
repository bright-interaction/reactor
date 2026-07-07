---
id: i_step-keys
topic: idempotency
title: Set StepOpts.IdempotencyKey on every side-effecting step
created_by: seed
sources: [https://stripe.com/docs/api/idempotent_requests]
tags: [idempotency, replay, side-effects]
gold: true
citation_count: 0
---

# Set StepOpts.IdempotencyKey on every side-effecting step

Replay reruns a workflow from a checkpoint. If a step that already
sent an email runs again on replay, the customer gets two emails.
Idempotency keys solve this: the supervisor records the key with
the step output, and re-running the closure with the same key
returns the cached output instead of executing the side effect.

The downstream service (Stripe, Resend, your own API) also gets
the key as a header, so if Reactor crashed mid-send and the network
delivered the request anyway, the second send is a no-op at the
upstream.

## Pattern

```go
out, err := reactor.Step(flow, ctx, "send-welcome", reactor.StepOpts{
    IdempotencyKey: customerID + ":welcome",
    Timeout:        30 * time.Second,
}, func(ctx context.Context) (Sent, error) {
    req, _ := http.NewRequestWithContext(ctx, "POST", resendURL, body)
    req.Header.Set("Idempotency-Key", customerID + ":welcome")
    // ... call api, return response
})
```

The same key on both sides:
- StepOpts.IdempotencyKey: the supervisor's journal cache key
- HTTP Idempotency-Key header: the downstream service's dedup key

## Picking a key

The key should be:
- **Deterministic from inputs**: same inputs → same key. `customerID + ":" + operation` is the canonical shape.
- **Unique to the action**: `customerID:welcome` not just `customerID` (the customer might trigger many actions over time).
- **Bounded**: under 255 chars (most APIs enforce a limit).

DO NOT include:
- Timestamps (kills determinism)
- Random values (kills determinism; use SideEffect if you must)
- The current attempt number (the supervisor already retries with the same key)

## When you can skip the key

Pure reads (GET requests, database SELECTs) don't need keys,
re-running them produces no side effect.

Steps that compute purely from inputs (parse, transform, validate)
don't need keys; they're naturally idempotent.

Everything that touches an external system through POST / PUT /
PATCH / DELETE needs a key.
