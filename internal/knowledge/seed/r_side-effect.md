---
id: r_side-effect
topic: replay
title: Use reactor.SideEffect to journal non-deterministic values for replay determinism
created_by: seed
sources: [https://docs.temporal.io/dev-guide/go/foundations#side-effect]
tags: [replay, determinism, side-effect, time, random]
gold: true
citation_count: 0
---

# Use reactor.SideEffect for non-deterministic values

Replay re-runs the workflow body from the journal. Steps return
their cached output (no closure execution); the workflow body itself
runs from the top. If the body calls `time.Now()` or `rand.Intn(100)`
or `os.Getenv("FEATURE_FLAG")` on the first run vs replay, the values
differ and control flow diverges. Diverged replay = corrupt run.

`reactor.SideEffect` solves this by journaling the value once and
returning the same value on every replay.

## Pattern

```go
now, _ := reactor.SideEffect(flow, ctx, "captured-time", func() time.Time {
    return time.Now()
})
// Use 'now' freely in the workflow body. On replay, SideEffect
// returns the original captured value, not a fresh time.Now().

token, _ := reactor.SideEffect(flow, ctx, "trace-token", func() string {
    return uuid.New().String()
})
```

Each call needs a unique name within the workflow so the journal
can match the cached value to the replay-time call site.

## When to use SideEffect vs Step

| Use SideEffect for | Use Step for |
|---|---|
| time.Now, time.Since | API calls (HTTP, gRPC) |
| crypto/rand, math/rand | Database writes |
| os.Getenv reads | Email/SMS sends |
| uuid.New | Anything with a side effect outside the process |

The split: SideEffect is for **observations** (capturing
non-deterministic local state); Step is for **actions**
(side-effecting calls to external systems).

## What the lint catches

`reactor lint` rejects `time.Now`, `crypto/rand.Read`, `math/rand`,
`os.Getenv` calls in the workflow body **outside** of any function
literal. SideEffect's closure is a function literal, so the call
moves inside an exempt scope. Inside `reactor.Step` closures the
same exemption applies (Step's closure body is also a FuncLit and
its output is journaled, so it's safe).

## Why not just call time.Now inside a Step closure?

You can. The Step's output is journaled so replay returns the same
captured time. SideEffect is the lighter shape when you only need
the value, not a retry budget or timeout. Use Step when you need
the full machinery; use SideEffect when you just want a journaled
constant.
