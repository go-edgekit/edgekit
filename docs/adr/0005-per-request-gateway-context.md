# ADR-0005: Provide a per-request gateway context in addition to `context.Context`

- **Status:** Accepted
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

Lifecycle hooks need consistent access to matched-route information, selected upstream, timestamps, decisions, and outcomes. Extensions sometimes need to pass data between phases (e.g., identity from an authentication hook to an analytics hook). Go's `context.Context` already handles cancellation, deadlines, and propagation and should not be replaced.

## Decision

Create **one gateway-specific request context per inbound request** before executing registered HTTP middleware. Make it accessible from the request's standard context and pass the same gateway context to lifecycle hooks. Its lifetime ends with gateway-managed completion.

This architectural direction is accepted; exact Go types, accessor names, and state-key functions remain provisional.

### Metadata and extension state

- Keep gateway metadata private, with a small read-only public view: start time, matched route, and selected upstream when available. Final execution facts are supplied to completion as a snapshot; they need not be duplicated as mutable request-context fields.
- Store mutable request-scoped extension data on the gateway context, **not** on shared extension instances. Extension instances may own separately synchronized cross-request counters, caches, and other explicitly shared state.
- Use private, identity-based typed keys for extension-owned state. Do not expose a shared `map[string]any` keyed by extension-defined strings. The `NewStateKey`, `SetState`, and `State` functions implement this direction; their signatures stay provisional until v1.
- Do not depend on another extension's internal fields. Any cross-extension state-sharing contract must be explicit.

### Concurrency and lifetime

Requests execute concurrently, and extension instances may be shared between requests. Their configuration and shared state must support that concurrency.

Within one request, gateway lifecycle hooks execute **synchronously and serially**. Request-context access is not synchronized, and extensions must not access it or its stored mutable values concurrently. Registered middleware must finish downstream handling before returning and invoke it at most once. Middleware such as `http.TimeoutHandler`, which can return while downstream work continues, is outside this contract.

A mutex around the state map alone would not make asynchronous middleware safe: completion could still precede downstream work, and mutable stored values could still race. Future concurrent callbacks require revisiting lifecycle ownership and completion, not merely adding a map lock.

Extensions must not retain the request context or its mutable state after completion. Independent record snapshots may outlive request processing.

### Standard context and tracing

Retain Go's `context.Context` for cancellation, deadlines, and tracing propagation; do not embed it in the gateway context or keep a second authoritative mutable copy there. Middleware must derive contexts from the incoming request context so the gateway context remains accessible.

Inner lifecycle hooks receive the current execution context. Completion receives the gateway entry context, not whichever derived context reached an inner handler. That entry context may already be canceled.

Extensions needing correlation at completion must explicitly snapshot identifiers or other required data while the middleware-derived context is available. Tracing middleware owns its span lifetime. A span may already have ended by completion; retaining or annotating a live span is a separate requirement, not an implicit completion guarantee.

## Consequences

**Positive:** Predictable lifecycle state; supports phases without repeatedly reconstructing metadata; still interoperates with Go HTTP code.

**Negative:** Serial request-state access restricts asynchronous middleware; trace correlation needs an explicit snapshot; future concurrent callbacks require a lifecycle design review; the generic state API still needs to remain evolvable.

## Alternatives considered

- **Only `context.Context` values:** Excellent for propagation, but cumbersome for evolving shared proxy lifecycle state.
- **A shared untyped string-key map:** Easy initially, but prone to collisions and implicit contracts.
- **Global mutable request state:** Unsafe across concurrent requests.

## Validation / follow-up

Tests verify the same gateway-context identity in global middleware, route middleware, and hooks, as well as typed-key isolation, stored nil values, cancellation, and state isolation across concurrent requests.

Middleware-derived trace identifiers must remain available in completion records, although completion receives the entry context after tracing middleware unwinds. The access-log experiment of the first implementation (tag `prototype-final`) showed this; the access-log extension must keep it under test. Explicitly review this ADR before introducing per-request concurrent callbacks or asynchronous middleware.
