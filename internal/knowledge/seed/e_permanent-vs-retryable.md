---
id: e_permanent-vs-retryable
topic: errors
title: Wrap step errors with reactor.Permanent or Retryable so the supervisor knows whether to retry
created_by: seed
sources: [https://pkg.go.dev/github.com/brightinteraction/reactor/sdk]
tags: [errors, retry, dlq, supervisor]
gold: true
citation_count: 0
---

# Wrap step errors with Permanent or Retryable

The supervisor decides whether to retry a failed step based on the
error's wrap. An unwrapped error is treated as Retryable by default
(safe but sometimes wrong). The two explicit wrappers make intent
unambiguous and make replay deterministic.

## reactor.Permanent

Wrap an error with `reactor.Permanent(err)` when re-running the
step would produce the same failure. The supervisor stops retrying,
moves the run to `failed_dlq`, and an operator picks it up via
`reactor dlq`.

Use for:
- Validation errors (input is malformed)
- 4xx responses other than 429 (server says no, retry will not help)
- Authentication errors (401, 403)
- Schema mismatches

```go
if resp.StatusCode == http.StatusBadRequest {
    return out, reactor.Permanent(fmt.Errorf("api rejected payload: %s", body))
}
```

## reactor.Retryable

Wrap with `reactor.Retryable(err)` when the failure is transient.
The supervisor applies the StepOpts retry budget (with backoff)
before giving up.

Use for:
- Network errors (connection reset, timeout)
- 5xx responses
- 429 Too Many Requests
- Database deadlocks

```go
if resp.StatusCode >= 500 {
    return out, reactor.Retryable(fmt.Errorf("upstream %d: %s", resp.StatusCode, body))
}
```

## Default behaviour (unwrapped error)

An unwrapped error is treated as Retryable. This is forgiving but
risks burning the retry budget on a permanent failure. Always
wrap; the cost is one function call.

## DLQ flow

When a step exhausts retries OR returns a Permanent error on its
last attempt:

1. Supervisor writes `dead_letter` row + flips run status to `failed_dlq`
2. Auto post-mortem fires (Ship 3): Claude analyses the run journal,
   writes a knowledge entry under `post-mortems/`, commits
3. Operator runs `reactor dlq retry <id>` after fixing the underlying
   issue; the supervisor re-runs the run with the same RunID, the
   journal cache short-circuits previously-succeeded steps, the
   failed step re-executes, success removes the dead_letter row

The post-mortem step is what makes the corpus compound: every
failure becomes a permanent reference for the next generation.
