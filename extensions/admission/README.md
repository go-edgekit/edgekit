# Fail-fast concurrency admission

`admission.New(maxActive)` returns synchronous gateway middleware that bounds concurrent calls to downstream handlers. It has **no waiting queue**. A positive limit is required; zero and negative values panic at construction.

```go
admit := admission.New(64)
app, err := gateway.NewWithConfig(gateway.Config{
    Routes: routes,
    Middlewares: []gateway.Middleware{
        access.Middleware, // Observe both admitted and saturated requests.
        admit,
    },
    Extensions: []any{access},
})
```

Here `admission` is `example.com/go-gateway-starter/extensions/admission`. The example executable uses this order and a limit of 64. These are example settings, not framework defaults. No new hook or gateway configuration field is needed.

## Behavior

- Every admitted call invokes `next` synchronously and once, with the original request and response writer. It does not buffer bodies, change contexts, set deadlines, or hide writer capabilities.
- A slot lasts until downstream returns or finishes unwinding. Uploads, streamed responses, and transport pool waits consume slots while their handlers are active. Context cancellation alone does not release a slot early; downstream must actually end. Panics, including `http.ErrAbortHandler`, propagate unchanged after releasing the slot.
- When all slots are occupied, the middleware sends a generic local 503 without invoking downstream or reading/closing the request body. The server still owns incoming-body cleanup. There is no waiting goroutine, admission queue, retry, or `Retry-After` policy.
- This counts handler calls, not TCP connections, so concurrent HTTP/2 streams share the same budget. Transport connection limits remain a separate control.
- Reusing one returned middleware shares a budget across all handlers it wraps. Separate `New` calls create independent budgets, useful for application-owned route groups or streaming workloads. There is no process-wide registry or cross-instance limit.

Register it inside the gateway, after HTTP observation and before work to be limited. Global registration rejects before request hooks and routing, leaving route/upstream metadata unset. Route-scoped registration runs after matching and request hooks, so those stages are outside that route's budget. Completion still runs once outside the middleware chain, after unwinding. A local 503 does not add a core error; the access-log extension currently interprets it as `LocalResponse`, not evidence of a proxy execution failure.

## Bounds and gaps

The limit covers only calls to `next`, not outer middleware, completion reporters, rejection writes, request-header parsing, accepted connections, or server cleanup after handler return. A slow reader can block a rejection write without holding a slot. A server can drain an unread request body after the middleware returns. Thus the limit is not a hard cap on all live HTTP goroutines, memory, or sockets.

There is no admission waiting queue, but admitted requests can still wait inside a transport pool; the number of such requests is constrained by the downstream-call budget. Cancellation must be honored by that transport and other downstream work. Separate request-rate control is not implemented.

Long-lived or stalled bodies can occupy all slots indefinitely. Use workload-specific slow-upload/slow-reader and connection policies at the serving edge; select independent budgets for streaming traffic where necessary. Do not add blanket response deadlines, asynchronous middleware, or `http.TimeoutHandler` to make slots expire before downstream ends. Manually hijacked work continuing after handler return is outside this budget.

Tests cover saturation without queuing, request-body ownership, shared/independent budgets, writer capabilities, concurrent callers, panic/cancellation release, gateway completion and local-response observations, incremental uploads, HTTP/2 streams on one connection, and cancellation during saturated transport-pool acquisition. See [the resource-limit assessment](../../docs/investigations/resource-limits.md) for configuration ownership and remaining slow-body work.
