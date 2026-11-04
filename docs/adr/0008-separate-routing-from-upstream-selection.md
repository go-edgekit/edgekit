# ADR-0008: Separate route matching from upstream selection

- **Status:** Accepted (2026-11-01)
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

A route represents which gateway behavior applies to an incoming request. An upstream selection chooses where that request goes. Initially, one static upstream per route is enough, but later extensions may implement load balancing, service discovery, weighted routing, or tenant-aware upstream decisions.

## Decision

Treat **route matching** and **upstream selection** as distinct conceptual stages:

- Route matching identifies the configured route and its associated middleware/hooks.
- Upstream selection determines the destination for the proxied request.
- For the scaffold, upstream selection defaults to the route's single static target.
- Do not put service discovery, health checks, retries, or load-balancing state in the route matcher.

Only add a public `UpstreamSelector` interface when a concrete alternative to static selection needs it. In the meantime, keep these responsibilities separable internally.

## Consequences

**Positive:** Maintains a natural path to dynamic routing and traffic management; keeps matching logic simple and deterministic.

**Negative:** Two stages introduce another concept for gateway authors; selected-upstream metadata must be clearly available to subsequent hooks.

## Alternatives considered

- **Always hardcode upstream on the route:** Simplest today but makes richer selection harder to introduce cleanly.
- **Require a load-balancing abstraction from day one:** Overdesign for a single-upstream prototype.

## Validation / follow-up

Test two static routes, and a custom selector that chooses between two upstreams without changes to route matching or the reverse-proxy implementation.

**Accepted 2026-11-01:** The core defines the upstream-selector extension point, with static selection as the default. Bundled load balancing and health checks remain deferred.

**Note 2026-11-03:** The condition in the decision is met. The first concrete alternative is a selector that uses request data from route middleware, such as a region. The public `UpstreamSelector` interface exists, with `StaticUpstream` as the default.
