# dd-trace-go v2 Orchestrion GLS span leak — minimal, contrib-independent repro

This reproduces the goroutine-local-storage (GLS) span leak that shows up in Orchestrion builds of `dd-trace-go/v2`. There's no contrib in it. No franz-go, no kafka, no http, nothing. The leak sits in the core tracer, in the `ContextWithSpan` push paired against the `Span.Finish` pop, so a handful of plain `tracer` calls is enough to trigger it.

It lives in the same area as [DataDog/dd-trace-go#4808][pr] (GLS push/pop accounting). Heads up, though: as the numbers further down show, that PR doesn't actually clear this particular case.

[pr]: https://github.com/DataDog/dd-trace-go/pull/4808

## TL;DR

```
# baseline: GLS off, nothing leaks
go run . -n 200000

# leak: orchestrion turns the GLS on
orchestrion go run . -n 200000
```

| Build (200k records)   | retained heap objects | per record |
|------------------------|-----------------------|------------|
| plain `go run .`       | ~200 (GC noise)       | ~0         |
| `orchestrion go run .` | ~3,000,000            | ~15        |

Same code both times. The Orchestrion build holds onto ~360 MB and ~3M objects that survive a forced GC, while the plain build stays flat. Goroutine count barely moves (17 → 17 under orchestrion), so it isn't a goroutine leak. The exact counts wobble a little run to run; the shape doesn't.

## The bug

In Orchestrion builds, `dd-trace-go` keeps a per-goroutine stack of the active spans (the "GLS"). Two core calls drive it (line numbers from `v2.8.2`):

- **push**: `tracer.ContextWithSpan(ctx, s)` → `internal/orchestrion.CtxWithValue` → `getDDContextStack().Push(...)`. The push happens on whatever goroutine calls `ContextWithSpan`, with no nil guard (`internal/orchestrion/context.go:35-42`).
- **pop**: `(*Span).Finish()` → `internal/orchestrion.GLSPopValue(...)`. The pop happens on whatever goroutine runs `Finish` (`ddtrace/tracer/span.go`, `internal/orchestrion/context.go:48`).

Each push and pop is scoped to the goroutine that made the call. So the stack only balances when the goroutine that pushed a span is also the one that finishes it. Push a span with `ContextWithSpan` on goroutine A, finish it with `Finish` on goroutine B, and A's push never gets popped. B just pops its own stack. That one span sticks around on A's GLS stack for good, once per re-injection. On a long-lived worker it adds up into a real leak.

#4808 reworks this: it grabs a goroutine-scoped popper at push time and invokes it once on finish, so the pop only fires on the goroutine that pushed. That stops a double finish from double-popping, and stops a finish on the wrong goroutine from corrupting that goroutine's stack. It does not make the leak above go away, though. When the pushing goroutine never finishes the span itself (someone else does, on another goroutine), the captured popper just no-ops on the finishing goroutine and the pushed entry stays put. See "What #4808 changes here" below.

## How this repro triggers it

`main.go` sets up the cross-goroutine case with nothing but `tracer` calls:

- an **owner goroutine** creates each span and finishes it on its own goroutine. That's the part an auto-instrumented consume hook normally plays, owning the span's lifecycle.
- a **worker goroutine** only re-injects each span with `tracer.ContextWithSpan(baseCtx, span)`, then throws the result away.

Throwing the context away is deliberate. It leaves the GLS stack as the only thing that can hold memory between iterations, not some growing `context.WithValue` chain. That way the number you measure is the GLS leak and nothing else.

## Running it

### Prerequisites

- Go (module mode).
- Orchestrion: `go install github.com/DataDog/orchestrion@latest`.
- No Datadog Agent needed.

### Baseline (no leak)

```
go run . -n 200000
```

Without orchestrion, `ContextWithSpan` just calls plain `context.WithValue`. No GLS, the discarded context gets collected, and `retained heap objects` per record sits at ~0.

### Orchestrion (leak)

The repo is already pinned (`orchestrion pin` generated `orchestrion.tool.go` and the `go.mod` entries). Build and run through orchestrion:

```
orchestrion go run . -n 200000
# or
orchestrion go build -o gls-leak . && ./gls-leak -n 200000
```

`retained heap objects` per record jumps to ~15. That's one leaked `*Span` dragging along the ~14 objects it points at (trace, tags, baggage, and so on).

> Note: you only see the leak when you build with orchestrion. `orchestrion.tool.go` pulls in `dd-trace-go/orchestrion/all/v2`, the standard orchestrion bootstrap, and that's what switches the core GLS on at build time. The repro code itself still calls zero integrations; `all` is purely the build-time enabler. Drop it and the GLS goes away along with the leak, which is another way of showing the problem lives in the core tracer rather than any contrib.

## What #4808 changes here (and what it doesn't)

I ran the repro against the PR commit (`ed0c1c76`, pseudo-version `v2.9.0-dev.0.20260527133435-ed0c1c761872`):

```
go get github.com/DataDog/dd-trace-go/v2@ed0c1c761872be8b4d9d020eee9ad05667b13b3c
go mod tidy
orchestrion go run . -n 200000
```

The leak is still there: ~16 retained objects per record under orchestrion, basically the same as v2.8.2. The plain build stays at ~0 either way.

That tracks with the fix, and the PR says as much: it lists "cross-goroutine leak (not corruption) of the pusher's slot survives this fix" as a known residual gap, out of scope and tracked separately. `GLSPopFunc` captures a popper bound to the goroutine that pushed, and on any other goroutine the pop is a deliberate no-op (so it can't corrupt that goroutine's stack). Here the worker pushes and the owner finishes, so the pop no-ops on the owner and the worker's entry is never released. #4808 makes cross-goroutine finish *safe*; it doesn't reclaim a slot that was pushed on a goroutine which never finishes the span.

So if your code re-injects a span on one goroutine and lets a different goroutine finish it, #4808 on its own won't stop the growth.

## Real-world shape (why it matters)

A long-lived Kafka consumer with one worker goroutine per partition typically does this per record:

```go
span, _ := tracer.SpanFromContext(record.Context) // consume span made by the consume hook
ctx = tracer.ContextWithSpan(ctx, span) // re-inject on the WORKER goroutine
// ... handle the record ...
```

The instrumentation finishes the consume span on a different goroutine from the worker that re-injected it. One leaked GLS entry per record, and on a hot partition that just keeps climbing. The fix that actually works is to keep push and pop on the same goroutine: open a worker-owned child span and `defer child.Finish()` on the worker, so the worker both pushes and pops. That holds regardless of #4808.

## Environment used to capture the numbers above

- `dd-trace-go/v2` `v2.8.2` (and `v2.9.0-dev.0.20260527133435-ed0c1c761872`, the #4808 commit, where the leak persists)
- `orchestrion` `v1.10.0`
- Go `1.25`/`1.26`, `darwin/arm64`

## Files

- `main.go` — the reproduction (~80 lines, pure `tracer` calls).
- `orchestrion.tool.go` — standard `orchestrion pin` output (build-time enabler).
- `go.mod` / `go.sum` — pinned deps.
