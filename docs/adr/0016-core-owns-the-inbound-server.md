# ADR-0016: The core owns the inbound server configuration

- **Status:** Accepted
- **Date:** 2026-11-01
- **Decision owners:** Project maintainers

## Context

A gateway that is only an `http.Handler` leaves the creation and configuration of `http.Server` to the embedding application. Several secure defaults in [ADR-0012](0012-secure-defaults-and-configurable-protections.md) are server settings: header-read timeout, header size, HTTP/2 stream and flow-control limits, and progress-based idle deadlines. The [proxy implementation survey](../investigations/proxy-implementation-survey.md) shows that mature gateways harden this accept-and-parse edge before routing. If the embedder owns the server, these defaults exist only when the embedder copies example code.

## Decision

The core provides a hardened server, or server configuration, that applies the server-level secure defaults. It is the supported way to serve a gateway. Embedders can relax each setting explicitly, following ADR-0012.

Using the gateway as a bare `http.Handler` in another server remains possible. The documentation states that server-level protections, including idle deadlines, are then the embedder's responsibility.

TLS termination and certificate management are not decided here.

## Consequences

**Positive:** Secure defaults apply to the whole request path, not only to handling after parsing; R8 and R9 have an owner.

**Negative:** The core takes on server lifecycle (listening, graceful shutdown) that was example code. Two supported ways to serve, with different guarantees, need clear documentation.

## Alternatives considered

- **Embedder or distribution owns the server:** Keeps the core a pure handler, but server-level secure defaults could not be enforced and ADR-0012 would have to drop them.
