# ADR-0015: Evaluate limits per subject, independently, and consume at admission

- **Status:** Accepted
- **Date:** 2026-11-01
- **Decision owners:** Project maintainers

## Context

In the gateway that motivated this project, limits apply only per key, so a customer with several keys cannot share a limit. Rate-limit and quota policies cannot be defined separately because the lowest limit wins. The bundled access-policy extensions must avoid both problems without adding policy terms to the core. Terms are defined in the [access-policy glossary](../../extensions/policy/CONTEXT.md).

## Decision

- **Subjects:** Identity extensions publish the subjects of a request, such as `key:<id>` and `customer:<id>`. Limit extensions apply limits to subjects and never interpret them. Any hierarchy, such as team or plan, is another subject.
- **Independent evaluation:** Every applicable limit is evaluated independently. A request passes only when all limits allow it. Limits are never merged into one effective limit.
- **Consumption:** A request consumes from its limits only when all of them allow it. A denial does not consume, so one key cannot exhaust a sibling's shared customer quota. A store adapter may be best-effort under contention, but must document it.
- **Rate limits and quotas** share this model but differ in purpose: rate limits protect capacity with rolling windows and selectable algorithms (initially GCRA and a sliding-window counter); quotas grant calendar-period entitlements with exact counting.
- **Timing:** Consumption happens at admission, before proxying. A quota may refund at completion, based on its interpretation of the outcome. Analytics records are never a source for quota counts.
- **Limit source:** The limit extension asks a limit source which limits apply to the subjects on a route. A static configuration source comes first; store-backed sources follow.
- **Key authentication:** Keys come from a configurable header; query-parameter keys are off by default. Stores keep only key hashes. A public key ID, not the secret, forms the subject and appears in analytics. Missing and unknown keys receive the same 401 rejection.

## Consequences

**Positive:** Solves per-customer limits and separate rate/quota policies; limit extensions do not depend on the identity extension; new hierarchies need no code changes.

**Negative:** All-or-nothing consumption across distributed counters is expensive and may need store-specific atomic operations.

## Alternatives considered

- **A fixed key → customer hierarchy:** Solves today's case but not team, plan, or API levels.
- **Consume in order or always:** A denial by one limit would drain others.
- **Count quotas at completion:** Concurrent requests could exceed the quota.

## Validation / follow-up

Test the scenario of a customer with keys A and B at 100 r/s each and a shared 150 r/s customer limit, including denial without consumption. How route-specific limit settings reach the extension (route bindings or route middleware) is decided by this experiment, not here.
