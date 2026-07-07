---
id: w_hmac-verify
topic: webhooks
title: Verify webhook HMAC with constant-time compare and a body-size cap
created_by: seed
sources: [https://datatracker.ietf.org/doc/html/rfc2104, https://stripe.com/docs/webhooks/signatures, https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries]
tags: [webhooks, hmac, security, constant-time]
gold: true
citation_count: 0
---

# Verify webhook HMAC with constant-time compare and a body-size cap

Inbound webhooks carry HMAC signatures so the receiver can prove
the body came from the expected sender. Three things must be right
or the verification is broken:

1. **Cap the body before reading.** Use `http.MaxBytesReader` so an
   attacker can't force you to compute HMAC over a 50 MB payload
   just to reject it.
2. **Compare with `crypto/subtle.ConstantTimeCompare`.** A regular
   `==` or `bytes.Equal` returns early on the first mismatched byte,
   leaking signature bytes through timing.
3. **Detect the cap explicitly so the response code is 413, not 400.**
   `errors.As(err, &http.MaxBytesError{})` distinguishes "too large"
   from "I/O error".

## Skeleton

```go
const maxBody = 1 << 20 // 1 MiB

body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
if err != nil {
    var maxErr *http.MaxBytesError
    if errors.As(err, &maxErr) {
        http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
        return
    }
    http.Error(w, "bad request", http.StatusBadRequest)
    return
}

mac := hmac.New(sha256.New, sharedSecret)
mac.Write(body)
expected := mac.Sum(nil)

got, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Signature"), "sha256="))
if err != nil || subtle.ConstantTimeCompare(got, expected) != 1 {
    http.Error(w, "bad signature", http.StatusUnauthorized)
    return
}
```

## Per-provider quirks

- **Stripe**: header is `Stripe-Signature: t=<unix>,v1=<hex>`. Signed
  payload is `<t>.<body>` (note the dot separator). Honour a 5-minute
  skew window: reject if `now - t > 5min` to prevent replay.
- **GitHub**: header is `X-Hub-Signature-256: sha256=<hex>`. Signed
  payload is the raw body.
- **Generic**: pick a header name like `X-Webhook-Signature: sha256=<hex>`,
  signed payload is the raw body. Add a `X-Webhook-Delivery: <uuid>`
  for dedup.

## Replay defence

Even with a valid signature, a replayed delivery can be a problem
(double-charge, duplicate webhook fan-out). Record the delivery id
in a `webhook_deliveries` table with a unique constraint on
`(provider, delivery_id)`. On insert conflict, return 200 with a
no-op body (the sender stops retrying).

## In Reactor

The `internal/runtime/webhook` package implements all three of the
above. The package's MaxBodyBytes constant is the cap; the
verifyHMAC function is the constant-time check; the
RecordWebhookDelivery journal call is the replay defence. Trigger
generators should read this entry before adding a new provider.
