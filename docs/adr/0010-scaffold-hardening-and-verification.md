# ADR-0010: Build a verification-oriented scaffold without claiming production readiness

- **Status:** Superseded by [ADR-0012](0012-secure-defaults-and-configurable-protections.md)
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

A gateway lives on a security boundary and is responsible for HTTP forwarding behavior even when it has no authentication or quota features. A proof-of-concept should be useful for learning without implying that production-grade safety follows automatically from using `ReverseProxy`.

## Decision

Include automated tests and minimal operational hygiene in the first scaffolding milestone:

- Unit/integration tests using `httptest` and multiple route configurations.
- Request/response streaming and cancellation tests.
- Reverse-proxy behavior tests for methods, paths, query strings, headers, and upstream errors.
- Middleware and lifecycle hook ordering and early-return tests.
- Context creation and propagation tests.
- Graceful shutdown of the HTTP server.
- Structured logging without secrets; request IDs may be implemented as a small example middleware.
- Conservative server and transport timeout configuration, documented per use case.

Explicitly document that advanced security and production operations are **not** complete. Future reviews must cover forwarded-header trust, hop-by-hop headers, allowed upstream destinations / SSRF, resource limits, slow clients, proxy protocol/TLS, WebSocket or upgrade behavior, distributed failure modes, and load testing as applicable.

Do not implement billing, authentication, quotas, external state, admin APIs, or dynamic plugins solely to create test scenarios.

## Consequences

**Positive:** Makes the minimal system credible and easier to evolve; catches lifecycle mistakes early; avoids accidental claims of production maturity.

**Negative:** Testing protocol boundaries takes effort even before adding user-visible features.

## Alternatives considered

- **Only test a happy-path proxy request:** Insufficient for a security-facing networking component.
- **Hardening every advanced gateway feature before experimenting:** Would defeat the minimal-scaffold objective.

## Validation / follow-up

Run `go test ./...`, `go test -race ./...`, and `go vet ./...` on the scaffold. Document known gaps and avoid promoting the framework as production-ready until requirements and tests justify it.
