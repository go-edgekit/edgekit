# ADR-0001: Build an extensible gateway framework, not an API-management product

- **Status:** Superseded by [ADR-0011](0011-library-first-gateway-framework.md)
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

We want to explore building a Go HTTP API gateway with the potential to open-source it. Existing API-management gateways provide opinionated models for API keys, customers, policies, quotas, and analytics. Those models are useful defaults but can constrain more specialized requirements, such as shared customer-level limits across many API keys.

Our immediate goal is a small and understandable gateway **scaffold**, not a feature-complete API-management product.

## Decision

Build an **embeddable gateway framework** with a minimal core responsible for:

- Receiving HTTP requests and matching routes.
- Proxying HTTP requests and responses.
- Orchestrating well-defined lifecycle phases.
- Composing standard Go HTTP middleware and gateway-specific extensions.
- Initializing request-scoped gateway state.

The core will not define customers, users, credentials, policies, subscriptions, quotas, billing, storage schemas, or a mandatory external service. These belong to optional Go packages authored by users or maintainers.

The initial repository will contain a reusable Go package and a small executable demonstrating how to assemble it. The example executable is **not** an authoritative management control plane.

## Consequences

**Positive:** Small public surface; domain-neutral; straightforward to reuse; good basis for testing different extensions.

**Negative:** Users must assemble a binary and provide domain features themselves; initially less turnkey than an API-management product; extension APIs still need careful design.

## Alternatives considered

- **Build a complete API-management product first:** Rejected for scaffolding due to scope and premature domain assumptions.
- **Expose only an HTTP reverse proxy:** Too little structure to explore extensibility.

## Validation / follow-up

Prove that an example extension can be written outside the core package without modifying core source. Defer built-in authentication, rate limiting, quotas, persistence, and billing.
