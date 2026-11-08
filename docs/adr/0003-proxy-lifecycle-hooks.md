# ADR-0003: Expose a small, phase-oriented proxy lifecycle

- **Status:** Accepted; request-phase placement superseded in part by [ADR-0031](0031-route-first-request-bodies.md)
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

Tests verify that the hook lists are compiled at startup and run in the order of [ADR-0021](0021-hook-order-by-provider-position.md), and that response hooks and completion work without mandatory core HTTP accounting. `BeforeUpstream` hooks get the actual rewritten outbound request and can use state that route middleware set. A private transport wrapper provides the fallible boundary after `ReverseProxy.Rewrite`, which cannot return errors. Tests verify hook ordering, changes that the upstream observes, request isolation, error redaction, skipping the transport on failure, body closure, cancellation, concurrency, and completion after middleware unwinds or an outbound hook panics.

In the first implementation (tag `prototype-final`), an [outbound-signing experiment](https://github.com/go-edgekit/edgekit/blob/prototype-final/extensions/outboundsign/README.md) and an access-log experiment validated these phases.

Signing needs no route-matched hook. Add `RouteMatched` only when a requirement needs it, and then verify that route middleware establishes state before it runs. Preserve the ordering in ADR-0004, and do not stabilize Go signatures solely because the architectural direction is accepted or one experiment succeeds.

**Amended 2026-11-03:** [ADR-0019](0019-hook-outputs-and-destination-ownership.md) gives `BeforeUpstream` hooks a view of the rewritten outbound request whose only mutable part is the header map, so upstream selection owns the destination structurally. It also defines the header rules that the core applies after the hooks.

**Note 2026-11-04:** Upstream selection is an extension point (`UpstreamSelector`, [ADR-0008](0008-separate-routing-from-upstream-selection.md)). [ADR-0013](0013-typed-rejections-and-replaceable-renderer.md) defines the typed rejection and the replaceable renderer.

**Superseded in part 2026-11-04:** [ADR-0021](0021-hook-order-by-provider-position.md) replaces registration order in each phase. The first hook provider is the outermost one, as for middleware: the request phases run from the first provider to the last, and `UpstreamResponse` and completion hooks run from the last provider to the first.

**Registration and order 2026-11-07:** [ADR-0030](0030-typed-hook-phase-lists.md) supersedes ADR-0021. Hooks are registered in independent typed phase lists, each in execution order. Header-owner declarations are separate.
