# ADR-0021: Order hooks by the position of their provider

- **Status:** Accepted
- **Date:** 2026-11-04
- **Decision owners:** Project maintainers
- **Supersedes in part:** [ADR-0003](0003-proxy-lifecycle-hooks.md) and [ADR-0004](0004-compose-http-middleware-and-hooks.md) (the order of hooks in each phase)

## Context

ADR-0003 and ADR-0004 run the hooks of each phase in registration order, also the completion hooks. Middleware nests: the first middleware is the outermost one, it sees the request first, and it unwinds last.

A hook provider often takes part in more than one phase. With registration order in each phase, a provider that changes the request and the response does not nest. For example, a signing provider must sign the outbound request after the other providers changed it, so it is registered last. With registration order, its `UpstreamResponse` hook also runs last, so it verifies a response signature after the other providers changed the response.

Other proxies run the response direction in reverse. Envoy runs its decoder filters in configuration order and its encoder filters in reverse order. Skipper calls each filter once for the request, in the order of the route definition, and once for the response, in reverse order.

Users read the configuration, not the dispatch code. A rule that gives a list position one meaning for middleware and another meaning for hooks causes surprises.

## Decision

`Config.Hooks` lists hook providers from the outermost one, closest to the client, to the innermost one, closest to the upstream. This is the meaning that the list of middleware has.

The core derives the run order of each phase from the position:

| Phase | Direction | Run order |
| --- | --- | --- |
| `RequestReceived`, `BeforeUpstream` | Toward the upstream | First provider to last provider |
| `UpstreamResponse`, `RequestCompleted` | Back toward the client | Last provider to first provider |

- The position orders a provider only among the hook providers. The stage order of ADR-0004 and ADR-0018 does not change: for example, all global middleware runs before any `RequestReceived` hook.
- An error from a hook stops the hooks that come after it in the run order of its phase.
- A provider that occurs more than one time in the list has a position for each occurrence.
- `HeaderOwner` declarations have no order.
- `New` compiles the lists in run order, so request handling does no extra work.

Completion follows the same rule, although completion hooks only observe. An exception for one phase is a rule that users must learn, and a completion hook that runs last for the outermost provider agrees with middleware that unwinds last.

## Consequences

**Positive:** One mental model for middleware and hooks. A provider that changes the request and the response nests: the provider closest to the upstream changes the outbound request last and sees the upstream response first. A signing provider is registered last and works in both directions.

**Negative:** Within one provider list, the response phases run in an order that differs from the list. Completion hooks of observers, such as an access log and a metrics recorder, run from the last to the first.

## Alternatives considered

- **Registration order in each phase:** Simple to state, but providers do not nest, and the list means something other than the list of middleware.
- **Reverse order only for `UpstreamResponse`, registration order for completion:** It is an exception that users must learn, for a phase where the order seldom matters.
- **Numeric priorities:** Independent extensions cannot coordinate their numbers, and the effective order is not visible in the configuration.
- **A separate list for each phase:** A provider that takes part in more than one phase must be registered more than one time, and the registrations can disagree.

## Validation / follow-up

Tests verify the run order of each phase with two providers that take part in all phases, that an error stops only the later hooks in the run order, and that each completion hook gets its own snapshot when the hooks run in reverse order.
