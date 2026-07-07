---
id: h_timeout
topic: http-clients
title: Always pass context.Context with a timeout to outbound HTTP calls
created_by: seed
sources: [https://pkg.go.dev/net/http, https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/]
tags: [http, timeout, context]
gold: true
citation_count: 0
---

# Always pass context.Context with a timeout to outbound HTTP calls

`http.DefaultClient` has no timeout. A call to a slow or stalled
server hangs forever, holding the goroutine, the connection, and
any locks it took. Compose this across N callers and the daemon
runs out of file descriptors.

## The pattern

```go
ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
defer cancel()

req, err := http.NewRequestWithContext(ctx, "POST", url, body)
if err != nil { return err }

resp, err := httpClient.Do(req)
if err != nil { return err }
defer resp.Body.Close()
```

`http.NewRequestWithContext` wires the context into the underlying
transport. When ctx fires, the request returns immediately with a
`context.DeadlineExceeded` error and the connection is closed.

## Construct your own client

```go
var httpClient = &http.Client{
    Timeout: 30 * time.Second,           // wall-clock cap
    Transport: &http.Transport{
        DialContext: (&net.Dialer{Timeout: 5*time.Second}).DialContext,
        TLSHandshakeTimeout:   5*time.Second,
        ResponseHeaderTimeout: 10*time.Second,
        ExpectContinueTimeout: 1*time.Second,
        MaxIdleConns:          100,
        MaxIdleConnsPerHost:   10,
        IdleConnTimeout:       90*time.Second,
    },
}
```

The two layers (Client.Timeout + per-request context) backstop
each other. Per-request ctx is the sharper knife; Client.Timeout
catches code paths that forgot to pass one.

## In Reactor workflows

Inside an `reactor.Step` closure, use the ctx the closure
received. The supervisor enforces `StepOpts.Timeout` over the
top, so a misbehaving HTTP call gets cancelled when the step's
deadline fires, the closure returns an error, and the journal
records a clean failure instead of a hung run.

Set `StepOpts.Timeout` at least 2x the HTTP client timeout so the
HTTP layer has time to return a structured error before the
supervisor force-cancels.
