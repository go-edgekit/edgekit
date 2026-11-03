# Architecture Decision Records (ADRs)

These ADRs separate framework choices from specific API-management features. The 2026-11-01 direction review accepted the remaining proposed ADRs or superseded them with ADRs 0011–0016. See the [roadmap](../roadmap.md) for the planned order of work.

Acceptance does not freeze precise Go interfaces or mean every preferred extension point is implemented. The current APIs remain provisional, and implementation gaps are recorded in the relevant follow-up sections. See [the context map](../../CONTEXT-MAP.md) for the core and access-policy glossaries.

## ADRs

| ADR | Status | Decision | Main point |
| --- | --- | --- | --- |
| [0001](0001-framework-scope.md) | Superseded by 0011 | Framework scope | Minimal, embeddable proxy core; business features live in extensions |
| [0002](0002-use-go-standard-http-proxy.md) | Accepted | HTTP foundation | `net/http` and `httputil.ReverseProxy` rather than a custom networking stack |
| [0003](0003-proxy-lifecycle-hooks.md) | Accepted | Lifecycle hooks | Small optional interfaces compiled into ordered hook lists at startup |
| [0004](0004-compose-http-middleware-and-hooks.md) | Accepted | Middleware coexistence | Route middleware enters before route-matched hooks; completion surrounds the managed chain |
| [0005](0005-per-request-gateway-context.md) | Accepted | Request context | Typed extension state, serial per-request access, and explicit context lifetimes |
| [0006](0006-compile-time-go-extensions.md) | Accepted | Extension distribution | Compile-time Go packages before dynamic plugin systems |
| [0007](0007-immutable-runtime-pipelines.md) | Accepted | Configuration lifecycle | Validate and compile immutable request pipelines at startup; policy data changes at runtime |
| [0008](0008-separate-routing-from-upstream-selection.md) | Accepted | Upstream choice | Route matching is distinct from destination selection; selector point built in the rewrite |
| [0009](0009-outcomes-errors-and-early-responses.md) | Accepted (amended by 0013) | Outcome semantics | Core execution facts, middleware HTTP observations, extension interpretations; completion is not delivery |
| [0010](0010-scaffold-hardening-and-verification.md) | Superseded by 0012 | Verification | Test correctness/security boundaries, without claiming production readiness |
| [0011](0011-library-first-gateway-framework.md) | Accepted | Project direction | Library-first core with correctness and hardening; business policy in unprivileged bundled extensions |
| [0012](0012-secure-defaults-and-configurable-protections.md) | Accepted | Protections | Secure defaults on; deployment-dependent protections provided but off until configured |
| [0013](0013-typed-rejections-and-replaceable-renderer.md) | Accepted | Rejections | Rejections carry headers; replaceable renderer with a `problem+json` default |
| [0014](0014-multi-module-repository.md) | Accepted | Modules | Dependency-light core; bundled extensions with dependencies get their own modules |
| [0015](0015-access-policy-model.md) | Accepted | Access policy | Limits per subject, evaluated independently, consumed at admission |
| [0016](0016-core-owns-the-inbound-server.md) | Accepted | Serving edge | The core provides the hardened server; a bare handler loses server-level protections |

## Decision process

- **Proposed:** A candidate direction that can still change based on experiments.
- **Accepted:** Architectural direction explicitly approved; Go APIs may remain provisional and some implementation work may be deferred.
- **Superseded:** Replaced by a later ADR; link to its replacement.
- **Rejected:** Considered but not adopted.

Do not silently change the rationale of accepted ADRs. Write a new ADR to supersede a significant decision.

## Remaining provisional APIs and deferred work

1. Exact Go hook signatures, registration APIs, and optional function adapters (ADR-0003). The optional-interface model and startup compilation are accepted.
2. Concrete rejection and structured failure-provenance types (ADR-0009). Execution facts, HTTP observations, and interpretations have distinct owners; completion does not guarantee delivery.
3. State-key API details (ADR-0005). Private typed keys, serial per-request access, and entry-context completion semantics are accepted. Asynchronous middleware requires revisiting the lifecycle contract.
4. Route-specific hook registration (ADR-0004). Route middleware entering before route-matched hooks, registration-order hook execution, and outer completion are accepted.
5. Dedicated route-matched hooks and, when needed, a selector strategy (ADRs 0003 and 0008). These remain deferred. The outbound-signing experiment now exercises a provisional `BeforeUpstreamHook`; its exact signature is not stabilized.

## Experiment findings and next validation

The shared-context and access-log experiments demonstrate:

- One gateway context accessible from global middleware, route middleware, and existing hooks.
- Private typed state with isolation across concurrent requests.
- Upstream response observation and completion after normal handling, early exits, and aborts.
- Middleware-owned status/byte accounting and trace-correlation snapshots without expanding the core outcome API.
- Optional response-writer instrumentation tested for flushing, informational responses, streaming failures, and connection upgrades.

The access-log middleware changes an inbound request header that is forwarded upstream; it does not validate a hook on the actual rewritten outbound request.

The [outbound-signing experiment](https://github.com/go-edgekit/edgekit/blob/prototype-final/extensions/outboundsign/README.md) separately demonstrates:

- Route middleware supplying private typed state to a provisional `BeforeUpstreamHook` after selection and rewriting.
- The upstream receiving signatures for the rewritten destination, method, effective Host, and escaped URI, including base-path and query changes.
- Ordered fallible outbound hooks before transport execution, with body closure and no delegation on hook failure.
- Error redaction, controlled rejections, cancellation, concurrent route isolation, and completion after middleware unwinds, including outbound-hook panics.

Signing did not require a route-matched hook. Dedicated route-matched hooks, structured failure provenance, and a public selector remain deferred. When a route-matched hook is needed, test that route middleware establishes state before it runs. Do not treat interface stabilization, production signing security, live-span annotation, asynchronous middleware, or durable billing as established by the current experiments.
