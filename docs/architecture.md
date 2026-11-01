# Go API Gateway — Architecture & Design

**Status:** Historical — initial design exploration. The current direction is in [ADR-0011](adr/0011-library-first-gateway-framework.md) and the [roadmap](roadmap.md).
**Version:** 0.1

This document records the initial exploration. See the [ADRs](adr/README.md) for current decisions and implementation gaps, and the [gateway survey assessment](investigations/high-over-findings.md) for findings compared with those decisions. The assessment does not expand the scaffold scope or change ADR statuses.

## 1. Vision

Build a lightweight, extensible API gateway framework written in Go.

The gateway should provide a reliable HTTP proxying foundation while allowing developers to implement custom functionality through well-defined extension points.

Potential extensions include:

- Authentication and authorization
- Rate limiting and quotas
- Customer and API key management
- Usage tracking and billing
- Request and response transformations
- Load balancing and upstream selection
- Logging, metrics, and tracing

**The goal is to build a framework, not an opinionated API management platform.**

The core should remain independent of domain-specific concepts such as customers, API keys, subscription plans, or billing models.

The project should be designed with the possibility of eventually becoming open source.

## 2. Core design principles

### Minimal core

The gateway core should primarily handle HTTP serving, routing, reverse proxying, and orchestrating the request lifecycle.

Domain-specific functionality belongs in extensions.

### Extensibility as a first-class concern

Developers should be able to customize gateway behavior without modifying its core.

Extensions should be composable, independently testable, and implemented using ordinary Go packages.

### Lifecycle-oriented architecture

Rather than relying exclusively on traditional HTTP middleware, the gateway should expose well-defined extension points throughout the proxy request lifecycle.

This approach is inspired by Cloudflare's Pingora framework.

### Standard Go compatibility

Use Go's standard library where practical, especially `net/http` and `httputil.ReverseProxy`.

Support conventional `http.Handler` middleware alongside specialized lifecycle hooks.

Avoid custom abstractions when standard Go interfaces are sufficient.

### Incremental development

Start with a functional reverse proxy and a small extension API.

Add features and abstractions as concrete use cases demonstrate their necessity. Avoid designing a complete plugin ecosystem in advance.

## 3. Extensibility model

The gateway should support two complementary extension mechanisms.

### 3.1 HTTP middleware

Traditional Go HTTP middleware:

`func(http.Handler) http.Handler`

Intended for functionality that naturally surrounds HTTP request handling.

Examples:

- Request logging
- Request IDs
- Tracing
- General authentication
- HTTP security headers

Middleware should be composable and compatible with existing Go libraries.

### 3.2 Proxy lifecycle hooks

Specialized extension points allow components to participate in particular stages of proxy execution.

Proposed lifecycle phases:

| Phase                      | Purpose                                                    |
| -------------------------- | ---------------------------------------------------------- |
| Request received           | Inspect or reject incoming requests                        |
| Route matched              | Access route information and apply route-specific behavior |
| Upstream selection         | Choose an upstream destination                             |
| Before upstream request    | Modify the outbound request                                |
| Upstream response received | Inspect or modify upstream response headers                |
| Proxy error                | Handle upstream connection or proxying failures            |
| Request completed          | Record outcomes, timing, metrics, and usage                |

Some phases may require specialized interfaces rather than generic hooks, particularly upstream selection.

Lifecycle hooks should be optional. Extensions should only implement the phases they need.

The gateway must eventually define hook execution order, error propagation, short-circuiting behavior, and interactions between extensions.

Response-body streaming and transformations may require additional lifecycle phases later.

## 4. Request context

Introduce a gateway-specific request context, inspired by Pingora's per-request context.

A context should be created for each incoming request and remain available across the entire proxy lifecycle.

Potential responsibilities:

- Store request metadata and timing.
- Hold the matched route.
- Track the selected upstream.
- Allow extensions to share request-scoped state.
- Make execution information accessible to completion and error hooks.

The gateway context should complement, not replace, Go's standard `context.Context`.

The standard context remains responsible for cancellation, deadlines, and propagation.

The gateway-specific context holds proxy lifecycle state.

Extension-specific state should be isolated to prevent naming collisions and unintended coupling between extensions. The precise API for storing and retrieving this state remains to be designed.

## 5. Extension composition

Initially, extensions should be ordinary Go packages linked into the gateway at compile time.

There is no requirement for dynamic plugin loading, WASM, or separate extension processes.

Developers should be able to:

- Register extensions programmatically.
- Configure their execution order.
- Apply middleware globally or to specific routes.
- Combine extensions from different packages.
- Implement custom functionality without modifying gateway internals.

The framework should not force extensions to use a particular database, cache, identity provider, or configuration system.

## 6. Initial implementation scope

The first milestone is **a minimal, functional gateway demonstrating the architecture**.

Include:

- HTTP server with graceful shutdown.
- Basic routing to configured upstreams.
- HTTP reverse proxy using the Go standard library.
- Standard HTTP middleware composition.
- A small set of proxy lifecycle hooks.
- Per-request gateway context.
- Basic configuration.
- Example extensions demonstrating middleware and lifecycle hooks.
- Automated tests of routing, proxying, hook execution, and context propagation.

Explicitly out of scope:

- API key management
- Authentication and authorization implementations
- Rate limiting and quotas
- Persistent storage
- Distributed state
- Billing and usage events
- Administrative APIs
- Dynamic extension loading

The focus is on getting the architecture right before introducing business-specific functionality.

## 7. Open architectural questions

These should be explored through implementation rather than decided prematurely.

1. **Extension API:** Should lifecycle phases use small interfaces, registered functions, or a combination?
2. **Request context:** How should extensions safely store and retrieve their own request-scoped state?
3. **Execution order:** How should middleware and lifecycle hooks interact and execute in a predictable order?
4. **Error handling:** How can extensions reject requests, report errors, or customize responses?
5. **Routing:** Should route-specific extensions be registered directly on routes, or through a separate policy/configuration mechanism?
6. **Upstream selection:** Should this be a replaceable core strategy rather than a general lifecycle hook?
7. **Configuration:** How much should the core configure, and how much should extensions own?

## 8. Long-term direction

The framework may eventually support distributed rate limiting, sophisticated quotas, customer-aware policies, billing integrations, and advanced proxy capabilities.

However, these should be implemented as independent packages built on the extension API.

The success of the architecture should be evaluated by how easily new functionality can be implemented without changing the gateway core.
