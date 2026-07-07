---
id: g_zombie-context
topic: goroutines
title: Never start a goroutine without knowing how it will stop
created_by: seed
sources: [https://dave.cheney.net/2016/12/22/never-start-a-goroutine-without-knowing-how-it-will-stop, https://pkg.go.dev/golang.org/x/sync/errgroup]
tags: [goroutines, lifecycle, context, errgroup]
gold: true
citation_count: 0
---

# Never start a goroutine without knowing how it will stop

A goroutine that has no exit condition becomes a leak. Long-lived
processes accumulate leaked goroutines until the runtime crashes or
shutdown hangs forever waiting for them.

## The rule

Every `go func() { ... }` must satisfy at least one of:

1. The function returns within bounded time on its own.
2. The function honours a `context.Context` and returns when ctx is
   cancelled.
3. The function reads from a channel that is guaranteed to be closed.

If none apply, you have a zombie waiting to happen.

## Pattern: errgroup with context cancellation

```go
import "golang.org/x/sync/errgroup"

g, gctx := errgroup.WithContext(ctx)
g.Go(func() error {
    return doWork(gctx) // returns on gctx.Done() OR when work completes
})
g.Go(func() error {
    return doOtherWork(gctx)
})
if err := g.Wait(); err != nil {
    return err
}
```

`errgroup.WithContext` cancels gctx as soon as any g.Go returns
non-nil. Other goroutines see ctx.Done(), exit, and Wait() returns.
No leaks, no orphan goroutines surviving the parent function.

## Anti-pattern: fire-and-forget

```go
go func() {
    // No ctx, no done channel. This goroutine has no way to know
    // the parent has moved on. If doWork blocks on a network call
    // with no timeout, the goroutine lives until the daemon dies.
    doWork()
}()
```

In an HTTP handler this is doubly bad: the handler returns 200 to
the client immediately, but the goroutine keeps running in the
background indefinitely. Multiply by request volume = goroutine
explosion + memory growth + connection-pool exhaustion.

## In Reactor workflows

The SDK's `reactor.Step` already wraps the closure with retry +
timeout + journal-write. Don't spawn raw goroutines inside a Step
closure. If you need parallelism, use `errgroup.WithContext(ctx)`
where ctx is the one the Step closure received.

For background work that must outlive the Step, use a separate
Step that itself uses errgroup. That way the journal records the
parallel-work step's outcome and replay sees it.
