---
id: g_panic-recovery
topic: goroutines
title: Recover panics in spawned goroutines so one bug does not kill the daemon
created_by: seed
sources: [https://go.dev/blog/defer-panic-and-recover, https://pkg.go.dev/runtime/debug#Stack]
tags: [goroutines, panic, recover, resilience]
gold: true
citation_count: 0
---

# Recover panics in spawned goroutines

A panic in a goroutine kills the entire process unless that
goroutine has a `defer recover()`. The main goroutine's recover
does not catch panics from spawned goroutines.

For a long-running daemon (Reactor, the supervisor, the scheduler,
the HTTP server) one nil-pointer dereference in a workflow's
helper code can take the whole runtime down with it.

## The wrapper

```go
func goSafe(log *slog.Logger, name string, fn func()) {
    go func() {
        defer func() {
            if r := recover(); r != nil {
                log.Error("goroutine panic",
                    "name", name,
                    "panic", r,
                    "stack", string(debug.Stack()))
            }
        }()
        fn()
    }()
}
```

Use `goSafe(log, "rotation-tick", runner.Tick)` instead of bare
`go runner.Tick()`. The cost is one allocated closure; the
benefit is the daemon survives a buggy worker.

## When NOT to recover

Inside `reactor.Step` closures: let it panic. The supervisor's
subprocess boundary catches the exit code and journals the run as
failed. Recovering inside the Step would hide bugs from the
journal + DLQ.

The rule of thumb: recover at the goroutine root if the goroutine
is supposed to run forever (event loop, tick loop, listener).
Don't recover inside short-lived units of work where a panic is a
real signal that something is wrong with the caller.

## Surfacing the panic

`runtime/debug.Stack()` captures the stack of the panicking
goroutine. Attach it to the slog log line so the operator can
find the call site without restarting in debug mode. PII guard
applies: scrub the panic value if it might contain customer data.
