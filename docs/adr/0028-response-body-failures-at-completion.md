# ADR-0028: Record response-body read failures at completion

- **Status:** Accepted
- **Date:** 2026-11-06
- **Decision owners:** Project maintainers
- **Refines:** [ADR-0009](0009-outcomes-errors-and-early-responses.md), [ADR-0023](0023-failure-evidence-and-transport-facts.md), [ADR-0025](0025-core-protections-body-limit-admission-and-egress.md), and [ADR-0027](0027-request-trailers-at-the-end-of-the-body.md)

## Context

A final response can fail while its body streams, after the transport and the response hooks returned. `ReverseProxy` aborts the copy without its error callback. The response headers can already be committed, but the failure is still an execution fact. The existing phases do not name this stage.

## Decision

The core records each read error other than `io.EOF` from the final response body as a `Failure` in `PhaseResponseBody`, with the original error unchanged. The observer runs after the response hooks, on the goroutine that copies the response body. It never changes the gateway context from the transport's request-body reader goroutine. The first recorded outcome still wins. `Close` delegates to the body, and the observer does not buffer, read ahead, render a response, or recover a panic.

This includes a request-body overflow or an invalid late trailer that reaches the response reader, such as when a transport echoes the request body. A failure while the response streams keeps the committed response and the abort behavior. The phase does not cover response-write errors, which remain middleware observations, or upgraded connections. Completion is a snapshot; work that continues after it cannot change it.

## Consequences

`PhaseResponseBody` is appended to the phase constants, so existing numeric values stay the same. Its string is `response_body`. Consumers of completion must handle the new phase and must no longer assume that a streaming abort has no failure.

## Alternatives considered

- **`PhaseUnknown`:** The stage is known.
- **`PhaseRoundTrip` or `PhaseUpstreamResponse`:** The round trip and the response hooks have already returned.

## Validation / follow-up

Tests verify the original error, normal EOF, truncation, cancellation, body-limit and trailer failures, body closure, safe diagnostics, and no rendering after a commit.
