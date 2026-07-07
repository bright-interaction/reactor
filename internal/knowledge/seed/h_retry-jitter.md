---
id: h_retry-jitter
topic: http-clients
title: Retry transient failures with exponential backoff plus full jitter
created_by: seed
sources: [https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/, https://pkg.go.dev/github.com/cenkalti/backoff/v4]
tags: [http, retry, backoff, jitter]
gold: true
citation_count: 0
---

# Retry transient failures with exponential backoff plus full jitter

Naive retry (immediate, or fixed delay) creates thundering herds:
every failed client retries at the same instant, the server gets
hit again, fails again, all clients retry again. The server never
recovers.

Exponential backoff with full jitter fixes this. The AWS
architecture blog post (linked) is the canonical reference.

## The formula (full jitter)

```
attempt 0: random in [0, base)
attempt 1: random in [0, base*2)
attempt 2: random in [0, base*4)
attempt N: random in [0, min(cap, base * 2^N))
```

Different clients pick different sleeps from the widening window,
so retries spread out instead of stacking.

## In Go

```go
import "math/rand"

func backoff(attempt int) time.Duration {
    base := 100 * time.Millisecond
    cap  := 30 * time.Second
    exp  := time.Duration(1<<attempt) * base
    if exp > cap { exp = cap }
    return time.Duration(rand.Int63n(int64(exp)))
}
```

Note: `math/rand` is OK here because the jitter doesn't need
crypto-grade randomness. Inside a Reactor workflow body it
would still be banned by `reactor lint` for replay determinism;
use `reactor.SideEffect` to journal the chosen sleep.

## What to retry

- Network errors (connection refused, reset, timeout)
- 5xx responses (server-side transient)
- 429 Too Many Requests (honour `Retry-After` if present)

## What NOT to retry

- 4xx other than 429 (the request is malformed; retrying will fail again)
- 401 / 403 (credential or scope problem; retry will fail until human)

## Idempotency matters

Retry is only safe for idempotent requests. POST/PATCH need an
idempotency key (Stripe-style header, or `reactor.StepOpts.IdempotencyKey`)
so the server can dedup a re-delivered request. Without one, a
retry-after-timeout where the original actually succeeded will
double-charge / double-create.
