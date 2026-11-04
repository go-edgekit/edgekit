# ADR-0017: Map failures to statuses by their source

- **Status:** Accepted
- **Date:** 2026-11-03
- **Decision owners:** Project maintainers
- **Refines:** [ADR-0012](0012-secure-defaults-and-configurable-protections.md) (R5, error mapping)

## Context

RFC 9110, section 15.6.3, defines 502 Bad Gateway as the status that a gateway sends after it received an invalid response from an inbound server. A failure in extension code, such as a signing hook or an upstream selector, is not that. A gateway that sends 502 for every failure hides whether the gateway or the upstream failed, and metrics that count 502 then also count extension defects. Other proxies make the distinction: Pingora maps upstream-source errors to 502 and internal errors to 500, and Traefik uses 502 for network failures, 504 for timeouts, and 500 for errors that it cannot classify.

Lifecycle hooks add places where extension code can fail during proxying. The hook phase alone does not tell the source: an upstream response that is present does not make a bug in a response hook an upstream failure.

## Decision

The source of a failure sets the status of the generic failure response. The phase where the failure occurs is a fact; it does not set the status alone.

| Condition | Status |
| --- | --- |
| A processing hook returns an error | 500 |
| The outbound header check fails for an entry that middleware or a hook set (ADR-0019) | 500 |
| The upstream selector returns an error or a URL that does not obey its contract | 500 |
| The round trip to the upstream fails, including the upstream response-header limit | 502 |
| `ReverseProxy` reports a failure that the core cannot assign to a source, such as a protocol switch that the upstream did not accept | 502 |
| The upstream timed out (R5) | 504 |
| The client is gone (R5) | 499 |
| No upstream is available | 503 |
| A hook identifies an invalid upstream response | 502 |

The last four conditions need a typed signal: a typed dial or request-written failure (R10), a typed selector result, or a typed failure from a hook. The core never identifies them from error text. Without the typed signal, a failure gets the status of its source: a timeout or a cancellation in the round trip is a round-trip failure (502), and an error from a selector or a hook is 500.

All failure responses have a generic body: the status text, without failure details.

## Consequences

**Positive:** The status tells operators whether to look at the gateway and its extensions or at the upstream. Metrics that count 502 do not count extension defects.

**Negative:** Clients and dashboards that expect 502 for every gateway failure see 500 for failures in extension code.

## Alternatives considered

- **Status by hook phase:** Simpler, but it reports a bug in a response hook as an upstream failure.
- **502 for every failure:** It hides the source of the failure and contradicts RFC 9110.

## Validation / follow-up

Tests verify 500 for each processing-hook phase and for selector failures, and 502 for round-trip failures, also with a configured transport. Each typed signal needs tests that show that its status comes from the type, not from error text.

**Superseded in part 2026-11-04:** [ADR-0022](0022-rejections-and-the-problem-response.md) replaces the status-text body with the `problem+json` body of ADR-0013, whose `title` is the status text.

**Refined 2026-11-04:** [ADR-0023](0023-failure-evidence-and-transport-facts.md) defines the evidence for 499, 503, 504, and 502 and their precedence. A typed timeout in the round trip gives 504 without a write fact, and a client cancellation gives 499 in each processing stage. The protocol-switch failures get a phase and a status.
