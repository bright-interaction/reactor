---
id: c_blocks
topic: connectors
title: Shape data with the block arsenal (Switch, Merge, Filter, ...)
created_by: seed
sources: []
tags: [blocks, switch, merge, filter, transforms, data-shaping]
gold: true
citation_count: 0
---

# Shape data with the block arsenal

`sdk/blocks` is the typed, generic equivalent of n8n's Switch / Merge / Filter
/ Item List nodes. They are pure, non-mutating functions, so use them inside or
between Step closures to route, filter, dedupe, group, batch, and merge data.
They never do IO, so they do not need their own Step. Prefer them over
hand-rolled loops: the intent reads clearly and the behaviour is consistent.

## Routing (Switch vs If)

- One condition, two outcomes: a plain Go `if` or `blocks.If(cond, a, b)`.
- Many conditions to a named branch: `blocks.Switch`. Return a label, then
  `switch` on it. Name labels like branches: "high-value", "needs-review".

```go
route := blocks.Switch(order, []blocks.Case[Order]{
    {When: func(o Order) bool { return o.Total >= 1000 }, Label: "high-value"},
    {When: func(o Order) bool { return o.Refunded }, Label: "refunded"},
}, "standard")
switch route {
case "high-value": // ...
}
```

Cases are evaluated in order; the first match wins. Put the most specific cases
first. Use `blocks.SwitchValue` to map straight to a value (a price tier, a
queue name) instead of a label.

## Merging (the set ways)

There are exactly three correct shapes; pick by how the two sides relate:

- **By matching key** -> `blocks.MergeByKey(left, right, key, combine)`. Left-join:
  every left row is kept; a matching right row enriches it; unmatched left rows
  pass through unchanged (no silent drops, unlike n8n's default). Use this to
  attach data fetched for each item (e.g. enrich contacts with CRM details).
- **By position** -> `blocks.Zip(a, b, fn)`. Pairs index 0 with 0, etc., stopping
  at the shorter slice. Use only when both sides are already aligned.
- **Just concatenate** -> `blocks.Append(setA, setB, ...)`. Use when the sets are
  independent and order is "all of A then all of B".
- **Two objects/maps** -> `blocks.MergeMaps(base, override)`. Later maps win on
  key collisions.

Never merge by zipping when the sides can be out of order: that silently
mispairs rows. Merge by key whenever a stable id exists.

## Item-list transforms

`Map` (transform each), `Filter` (keep some), `Reduce` (fold to one),
`UniqueBy` (dedupe, keeps first occurrence, preserves order), `GroupBy`
(bucket by key), `KeyBy` (index by key, last wins), `SortBy` (returns a sorted
copy, input untouched), `Limit` (first N), `Chunk` (split into batches, e.g.
to respect an API's bulk-size limit), `Flatten`.

## Why this is robust

Every block returns a new value and never mutates its input, so a Step that
uses them stays deterministic and safe to replay. They are compile-checked
generics, so a type mistake fails the build, not production.
