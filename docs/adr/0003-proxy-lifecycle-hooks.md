# ADR-0003: Expose a small, phase-oriented proxy lifecycle

- **Status:** Accepted
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

Generic `http.Handler` middleware is excellent for cross-cutting HTTP behavior, but it cannot naturally control every proxy-internal event. Developers may need to inspect a matched route, select an upstream, rewrite the outbound request, inspect upstream response headers, and learn about failures. Pingora's phase-oriented design is a useful reference.

## Decision

Use explicit lifecycle **phases** as the primary gateway-specific extension model. Extensions opt into small optional interfaces. Compile ordered hook lists during gateway initialization, rather than discovering capabilities dynamically on every request. Function adapters may be added for convenience; they do not introduce a separate dispatch model.

The preferred extension boundaries are:

1. `RequestReceived` — after global middleware enters; before route matching.
2. `RouteMatched` — after route metadata is recorded and route middleware enters; before upstream selection. Route middleware can establish state used by these hooks.
3. `BeforeUpstream` — selected destination and the actual rewritten outbound request are available; before transport execution.
4. `UpstreamResponse` — upstream response headers received; before forwarding final downstream headers. Forwarded informational responses are outside this hook, except protocol upgrades (101).
5. `RequestCompleted` — after the gateway-managed handler chain has returned normally or finished unwinding after an abort. Completion observes execution, not guaranteed client delivery.

These names describe conceptual boundaries, not frozen Go interfaces. This architectural direction is accepted; precise signatures and registration APIs remain provisional. Add the missing extension points when concrete use cases exercise them, not merely to complete the list.

Upstream selection is a separate operation owned by one configured strategy, not a broadcast hook where several extensions compete to choose a destination. A static route target is sufficient initially; a public selector is deferred as described in ADR-0008.

Hooks run synchronously and serially within a request, in registration order, including completion hooks. A terminal rejection or error stops normal processing; completion still runs after the handler chain unwinds. Completion hooks are observational and must not panic or write responses.

One error-response renderer owns converting explicit rejections and unexpected failures into HTTP responses. Do not initially expose a separate public `ProxyError` notification hook: structured failure information supplied to completion is sufficient for observation. Add an error interception point only for a demonstrated need to change error handling. See ADR-0009 for ownership of facts, HTTP observations, and interpretations.

Keep successful local responses in HTTP middleware for now. The concrete typed-rejection API remains provisional; introduce a richer hook result only when a hook needs to produce a successful local response. Response-body chunk hooks are out of scope for the scaffold.

## Consequences

**Positive:** Explicit extension capabilities; known state availability per phase; no monolithic required interface; easier to add sophisticated plugins later.

**Negative:** More API design work than ordinary middleware; hook order, errors, and short-circuit rules require precision; some phases depend on `ReverseProxy` support.

## Alternatives considered

- **Only `http.Handler` middleware:** Simpler, but insufficient for upstream-specific behavior.
- **One large `ProxyHttp`-style interface:** Forces unrelated implementations to provide many no-op methods.
- **One generic event bus for all phases:** Loses type safety and makes control flow harder to understand.

## Validation / follow-up

The current implementation exposes request, upstream-response, and completion hooks, compiled at startup. The access-log experiment exercises middleware state, response hooks, and completion without expanding the core API. Route-matched hooks, outbound-request hooks, and structured failure provenance remain follow-up work, not existing capabilities.

Validate missing boundaries with an independent extension that uses matched-route state and modifies the actual rewritten outbound request. Changing an inbound header that is later forwarded does not validate the outbound hook. Preserve the ordering in ADR-0004, and do not stabilize Go signatures solely because the architectural direction is accepted.
