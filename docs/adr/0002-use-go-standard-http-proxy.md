# ADR-0002: Use the Go standard HTTP stack for serving and proxying

- **Status:** Accepted (2026-11-01)
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

HTTP reverse proxies are deceptively complex. The gateway must forward requests correctly, handle cancellation and streaming, manage connection reuse, and avoid reproducing subtle HTTP security and protocol behavior. Tyk's custom reverse-proxy implementation illustrates how such code can grow over time. Pingora offers a purpose-built proxy framework, but is written in Rust.

## Decision

Use `net/http` for the server and `net/http/httputil.ReverseProxy` as the initial proxy engine.

- Use `ReverseProxy.Rewrite` to prepare upstream requests and explicitly manage forwarding-header trust.
- Use `ModifyResponse` for upstream response inspection and header adjustment before forwarding.
- Use `ErrorHandler` for upstream round-trip failures.
- Treat `http.RoundTripper` as the transport extension boundary.
- Configure and reuse `http.Transport` instances; avoid constructing a transport per request.
- Forward bodies by streaming by default; do not buffer complete bodies solely to make hooks convenient.

Do **not** fork `ReverseProxy` or implement a custom HTTP stack in the first milestone.

## Consequences

**Positive:** Minimal dependencies; standard Go semantics; easier security updates; familiar APIs; reasonable HTTP performance without specialized networking code.

**Negative:** Some advanced proxy lifecycle operations (notably streaming body transformation, retries after body consumption, and protocol-specific control) may be awkward or unavailable via public hooks. We must not claim feature parity with Pingora.

## Alternatives considered

- **Fork `ReverseProxy`:** More control, but carries maintenance and security costs.
- **Use a third-party Go proxy engine:** Possible later if standard hooks prove insufficient.
- **Use Pingora:** Attractive design reference, but Rust adds a different runtime/language and doesn't solve our extension-domain problems.

## Validation / follow-up

Test streaming, client cancellation, upstream failures, query and path forwarding, forwarded-header behavior, and graceful shutdown. Benchmark only after establishing representative workloads.
