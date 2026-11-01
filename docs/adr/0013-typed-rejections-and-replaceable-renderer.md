# ADR-0013: Use typed rejections with headers and a replaceable error renderer

- **Status:** Accepted
- **Date:** 2026-11-01
- **Decision owners:** Project maintainers
- **Amends:** [ADR-0009](0009-outcomes-errors-and-early-responses.md) (rejections and local responses)

## Context

ADR-0009 kept `HTTPError` (status and message) as an experiment and said to add a richer result when a concrete hook needs it. Rate limits need that: a denial must return 429 with `Retry-After` and `RateLimit` headers.

## Decision

- A **rejection** carries a status, response headers, and a public reason code. It remains distinct from an unexpected failure.
- One central renderer converts rejections and failures into responses. The embedder can replace it.
- The default renderer writes an RFC 9457 `application/problem+json` body with minimal content. Failure details never reach the body.
- Headers on allowed responses, such as `RateLimit`, are added through the upstream-response hook, not through rejections.

ADR-0009's ownership of execution facts, HTTP observations, and interpretations is unchanged.

## Consequences

**Positive:** Standard error bodies; extensions can produce correct 429 and 401 responses without writing to the response writer.

**Negative:** The renderer and rejection types become public API that needs careful versioning.

## Alternatives considered

- **Let hooks write responses directly:** Rejected in ADR-0009.
- **Plain-text default body:** Simpler, but not a standard machine-readable format.
