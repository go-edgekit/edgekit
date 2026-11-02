# Gateway survey: findings for this project

**Status:** Investigation assessment, not a new architecture decision

## Summary

An earlier gateway survey supports our current direction: a small programmable Go proxy framework, not a smaller API-management platform. Its most useful findings concern extension boundaries, configuration ownership, and runtime safety. The survey is no longer in the repository, because its sources cannot be checked (see [Evidence limits](#evidence-limits)). This assessment keeps the findings that the project uses.

Its recommended release scope and API sketches should not be adopted as a package. They move toward a turnkey gateway distribution and introduce abstractions that our scaffold does not yet need.

This assessment compares the survey with the [ADRs](../adr/README.md), current implementation, and [access-log experiment](../../extensions/accesslog/README.md). It does not change ADR statuses, stabilize public interfaces, or expand the implementation milestone. ADRs 0003, 0004, 0005, and 0009 are accepted; the other existing ADRs remain proposed.

## Alignment with current plans

| Survey finding | Project direction | Status |
| --- | --- | --- |
| Build a framework rather than an API-management product | [ADR-0001](../adr/0001-framework-scope.md): domain features belong in optional packages | Proposed |
| Use standard Go HTTP primitives | [ADR-0002](../adr/0002-use-go-standard-http-proxy.md): `net/http`, `ReverseProxy`, streaming, and reusable transports | Proposed |
| Expose explicit proxy lifecycle boundaries | [ADR-0003](../adr/0003-proxy-lifecycle-hooks.md): small optional interfaces compiled at startup | Accepted |
| Preserve conventional HTTP middleware | [ADR-0004](../adr/0004-compose-http-middleware-and-hooks.md): middleware and hooks have separate responsibilities | Accepted |
| Keep request state separate from cancellation and deadlines | [ADR-0005](../adr/0005-per-request-gateway-context.md): private typed state alongside standard contexts | Accepted |
| Prefer native Go packages over dynamic plugins | [ADR-0006](../adr/0006-compile-time-go-extensions.md): explicit construction and compile-time integration | Proposed |
| Validate and compile configuration before serving | [ADR-0007](../adr/0007-immutable-runtime-pipelines.md): immutable startup pipelines | Proposed |
| Separate routing from destination selection | [ADR-0008](../adr/0008-separate-routing-from-upstream-selection.md): static targets first, selector when needed | Proposed |
| Treat proxy safety as more than feature completeness | [ADR-0010](../adr/0010-scaffold-hardening-and-verification.md): verify boundaries without claiming production readiness | Proposed |

These similarities strengthen the rationale for the current direction. They are not evidence that deferred capabilities are implemented.

## Recommendations not to adopt now

### A production feature list as the scaffold milestone

The survey's first production release includes authentication, rate limiting, weighted balancing, health checks, retries, telemetry integrations, TLS/mTLS, declarative files, and atomic reload.

Those may be useful capabilities in an assembled gateway, but they are not prerequisites for validating the framework. Our immediate success criterion remains whether independently authored extensions can implement useful behavior without changing core source.

### One generic phase interface

The survey proposes `Handle(phase, ctx, next)` as a common extension interface. Keep conventional HTTP middleware and small optional hook interfaces instead:

- Different phases need different inputs and permissions.
- Authentication is a policy, not a required core lifecycle phase.
- Completion must surround gateway-managed handling, including early exits and aborts. It is not merely the last stage in a normal chain.
- A response-body phase name does not define streaming ownership, backpressure, cancellation, or framing behavior.

The accepted lifecycle direction is more precise than the survey's sketches. Add missing hooks only when a concrete extension needs them.

### Mandatory core accounting and telemetry dependencies

Interpret "observability as a core contract" as reliable execution boundaries and extension access, not mandatory response-writer instrumentation or an embedded telemetry SDK.

The access-log experiment already demonstrates useful accounting and trace correlation without expanding the core outcome. Preserve [ADR-0009](../adr/0009-outcomes-errors-and-early-responses.md): the gateway reports execution facts, middleware reports HTTP observations, and extensions derive interpretations. Completion is neither confirmed client delivery nor a durable usage event.

Likewise, do not copy the survey's `Principal` and `trace.Span` fields into the gateway context. They introduce identity and tracing assumptions that belong outside the minimal core. Gateway metadata should remain read-only to extensions, with extension-owned data isolated by typed keys.

### A registry merely because extensions are statically linked

Caddy is useful evidence for compile-time extensibility, but static linking does not require global registration, blank imports, schemas, or a provisioning system. Explicit constructors and registration already support dependency injection and clear ownership.

Revisit a registry only if configuration must select extensions by name. A reusable framework also needs an importable public API; the survey's runtime layout under `internal/` cannot be copied unchanged.

## Lessons to carry forward

### Separate framework, optional packages, and distribution

Keep three responsibilities distinct:

1. **Framework:** routing, proxy execution, request state, and extension contracts.
2. **Optional packages:** authentication, limits, telemetry, selectors, and other policies.
3. **Distribution:** an executable selecting packages, configuration format, dependencies, and operational defaults.

The existing `cmd/gateway` composition root demonstrates this separation. A future distribution can provide convenient defaults without turning them into framework requirements. No new distribution component is required now.

### Reload requires resource ownership, not only an atomic swap

Immutable snapshots are a useful future direction, but publishing a pointer is only part of safe reload. Before implementing reload, define:

- Cleanup for resources created during a failed candidate build.
- Retirement of old extensions, transports, and background workers after in-flight requests finish.
- Ownership and continuity of mutable health, cache, and rate-limit state.
- Ordering or conflict handling for overlapping configuration updates.

Keep startup compilation now. Do not add reload or an admin API merely because the snapshot design could support them later.

### Runtime safety deserves a separate validation effort

A minimal feature set is not a production-safety guarantee. The example server has header-read and idle timeouts, not a complete production configuration.

A focused hardening pass should cover forwarded-header trust, hop-by-hop headers, explicit upstream timeout and connection-pool configuration, request-size limits, slow clients, cancellation, upstream destination restrictions, and sensitive-path/error redaction. Decide placement between framework, middleware, and executable for each requirement. Test timeout policies against streaming rather than applying blanket defaults.

### Additional extension runtimes are operational commitments

WASM and external processors introduce packaging, compatibility, resource limits, cancellation, and failure-policy contracts. Defer them until a documented need justifies those costs.

If external processing becomes necessary, design timeouts and fail-open/fail-closed behavior with the adapter. If retries become necessary, account for idempotency, body replayability, partial responses, and a total deadline; do not infer retry safety from the method alone.

## Recommended follow-up

1. Preserve the current scope and accepted lifecycle decisions. No architectural pivot is needed.
2. Select one concrete extension experiment for missing boundaries. An outbound-signing extension is a candidate: test route-derived state and access to the actual rewritten request before transport execution. Changing an inbound header does not validate that boundary. Fallible outbound work also needs more than `ReverseProxy.Rewrite`, which cannot return an error.
3. Run a focused proxy-hardening pass without adding business features just to create test scenarios.
4. Keep distribution features separate from framework requirements: file configuration, built-in policies, reload, and management APIs remain deferred possibilities, not committed milestones.
5. Do not use the survey's product-specific claims for a decision. Check them against primary sources first.

These are follow-up recommendations, not newly accepted ADRs or commitments to implement every listed capability.

## Evidence limits

The survey had citation markers, but no source URLs or bibliography. Its product-specific claims, licenses, and extension details were therefore not auditable and were not independently verified in this assessment.

Its warning that vendor throughput figures are not directly comparable is appropriate. Those figures do not establish a performance target for this project. The stated 90%/9% extension split is explicitly a design target, not measured industry evidence.

Use these findings as architectural context. Use our experiments to establish local behavior, and representative benchmarks and security tests to assess future operational claims.
