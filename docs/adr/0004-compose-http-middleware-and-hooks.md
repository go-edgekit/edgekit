# ADR-0004: Support standard HTTP middleware alongside lifecycle hooks

- **Status:** Accepted
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

Go already has a common middleware convention: `func(http.Handler) http.Handler`. Replacing that model would prevent reuse of existing middleware, but relying on it exclusively would hide proxy-specific phases. Global and route-scoped middleware also need predictable context availability and ordering.

## Decision

Support both mechanisms with distinct responsibilities:

- **HTTP middleware:** Wraps general HTTP processing, supports ordinary `http.Handler` packages, and may short-circuit requests.
- **Lifecycle hooks:** Execute inside the gateway at documented phases, with access to proxy-specific data.

The gateway controls the composition root. The accepted ordering for gateway-managed handling is:

```text
Initialize gateway request context and install completion observer
  Global HTTP middleware: before
    RequestReceived hooks
    Match route and record route metadata
    Route HTTP middleware: before
      RouteMatched hooks
      Select upstream
      Prepare outbound request
      BeforeUpstream hooks
      Proxy request / UpstreamResponse hooks / response streaming
      Centralized error rendering when applicable
    Route HTTP middleware: after
  Global HTTP middleware: after
RequestCompleted hooks in the outer completion observer
```

The normal path is shown above. Rejection, routing misses, errors, or aborts can skip later stages. Middleware unwinds around the stages it entered, and completion remains outside that chain.

Global middleware may run before a route is known. Route middleware enters after route metadata is available but **before route-matched hooks**, allowing it to establish state those hooks use. Initializing the gateway context outside registered middleware makes the same gateway context available to both middleware and hooks.

Middleware follows nesting order: the first registered middleware is outermost and unwinds last. Lifecycle hooks run serially in registration order, including completion hooks. Hook errors and explicit rejections stop normal processing; middleware that does not call `next` prevents inner hooks from running. The exact Go interfaces and route-specific hook registration API remain provisional.

Registered middleware must invoke downstream handling synchronously and at most once. Middleware that returns while downstream work continues, such as `http.TimeoutHandler`, is outside this lifecycle contract. See ADR-0005 for state ownership and concurrency rules.

Completion observes when the **gateway-managed handler chain has returned normally or finished unwinding after an abort**, including rejection by registered global or route middleware. It does not mean that the client received the entire response. Streaming failures, connection upgrades, or client disconnects can limit what is known. Completion hooks must not panic or write responses.

Middleware wrapping the gateway externally is outside this guarantee. The gateway context is not available to it before entry, and response writes or recovery after gateway handling ends cannot be observed retroactively by gateway completion.

HTTP response accounting belongs in optional response-observing middleware, not mandatory core instrumentation. See ADR-0009 for the distinction between execution facts, HTTP observations, and interpretations.

## Consequences

**Positive:** Reuses synchronous conventional Go middleware; supports proxy internals cleanly; gives route middleware a clear state-initialization boundary before route hooks.

**Negative:** Two composition mechanisms may confuse users; outer middleware can prevent inner hooks from running; asynchronous middleware is not supported by this contract; optional response capture requires care with `http.ResponseWriter` capabilities.

## Alternatives considered

- **One universal middleware API:** Simpler headline, but too restrictive for upstream handling.
- **No HTTP middleware compatibility:** Unnecessary friction for Go consumers.

## Validation / follow-up

Existing tests cover normal middleware ordering, route misses and redirects, global and route rejection, panic unwinding, and completion. The access-log tests exercise optional response capture, streaming, flushing, and connection upgrades.

When route-matched hooks are added, test that route middleware establishes state before those hooks run. Keep architectural ordering distinct from which extension points are currently implemented, and verify early exits skip only the inner stages.
