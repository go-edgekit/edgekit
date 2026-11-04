# ADR-0009: Model request outcomes and early termination explicitly

- **Status:** Accepted
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

A request can complete normally, be rejected by middleware, miss a route, fail to reach the upstream, or be canceled by the client. An extension may need to respond without proxying. Analytics and future billing integrations need reliable distinctions between these cases. A single generic `error` is not enough to define the HTTP response or outcome.

## Decision

Keep three kinds of information separate:

> The gateway reports execution facts; middleware reports HTTP observations; extensions derive application-level interpretations.

This ownership boundary is accepted. Exact Go outcome, failure, and rejection types remain provisional.

### Execution facts: gateway-owned

Keep a small gateway-owned view of facts that gateway execution can establish, such as:

- A route was matched, or routing found no route.
- An upstream was selected.
- An upstream response was received.
- A lifecycle hook explicitly rejected further processing.
- A known operation failed, with its phase and cause when available.
- Handling returned normally or unwound abnormally.
- Timing information.

A selected upstream is **not** proof that it was contacted or received the request. An observed cancellation is not proof that cancellation caused the final result. Do not infer facts the transport or lifecycle does not establish.

Completion receives the available execution facts alongside read-only gateway metadata. Add structured failure provenance as concrete requirements exercise it; do not claim every streaming or connection-upgrade failure is visible through the proxy error callback. No separate public error-notification hook is required initially.

### HTTP observations: middleware-owned

Optional response-observing middleware owns downstream HTTP status, body bytes accepted by the response writer, and response-write errors. These observations do not require mandatory core response-writer instrumentation or fields in the core outcome model.

An upstream status observed in a response hook and a downstream status observed in middleware are different observations: a later hook may modify or reject the upstream response. Status and byte counts do not prove complete client delivery, and raw hijacked traffic may bypass the response writer entirely.

### Interpretations: extension-owned

Extensions may combine facts and HTTP observations into classifications for logging, metrics, analytics, or billing. Those classifications are not authoritative gateway facts.

For example, an ordinary middleware response of 403 does not establish why it was produced. A normal upstream 503 is an HTTP response, not necessarily a proxy execution failure. A streamed response can have status 200 and accepted body bytes before an abort. Do not flatten these distinctions into one core enum mixing rejection reasons, HTTP results, and inferred failure causes.

### Rejections and local responses

Provide an explicit typed rejection understood by one centralized error-response renderer. A controlled rejection is distinct from an unexpected failure, even if both currently use an `error` return. Stop normal processing and render a rejection or failure only where the response can still be changed; do not attempt a replacement response after final headers are committed.

The `HTTPError` of the first implementation (tag `prototype-final`) was an experiment, not a frozen public contract; [ADR-0013](0013-typed-rejections-and-replaceable-renderer.md) replaces it. Keep successful local responses in HTTP middleware. Introduce a richer hook result only when a concrete hook needs to produce a successful local response, rather than making every hook write a response directly.

### Completion guarantee

Invoke the outer gateway-owned completion observer once after the **gateway-managed handler chain has returned normally or finished unwinding after an abort**, including registered middleware rejections, routing misses, and proxy errors. Completion hooks are observational and must not panic or write responses. The synchronous execution contract in ADR-0005 is required for this guarantee.

Completion means gateway-managed execution has ended. It does **not** mean the client definitely received the entire response. This distinction applies to streaming, connection upgrades, client disconnects, and future billing integrations. Completion is not a durable event queue, a delivery acknowledgment, or an exactly-once usage/billing event. Failures or recovery outside the gateway cannot be observed retroactively.

Avoid generic retries by default; retry policy must account for idempotency, body replayability, and partial upstream responses.

## Consequences

**Positive:** Keeps core facts separate from HTTP accounting and application policy; supports access logging without mandatory response-writer wrapping; avoids treating handler completion as confirmed delivery or durable billing.

**Negative:** Extensions must combine observations and own their classifications; exact failure phases may remain unknown; optional response-writer instrumentation and rejection APIs still require careful design.

## Alternatives considered

- **One rich core outcome enum plus mandatory HTTP accounting:** Mixes observable facts with inferred classifications and makes response-writer instrumentation a core responsibility.
- **Treat every hook `error` as an unexpected failure:** Loses the distinction between deliberate rejection and failure. An error return remains possible if its typed rejection semantics are explicit.
- **Have all hooks write `http.ResponseWriter` directly:** Easy to misuse after headers are committed and hard to compose reliably.

## Validation / follow-up

Tests cover success, local rejection, routing misses, connection failures, canceled contexts, partial writes, streaming aborts, and connection upgrades. Response-observing middleware combines its HTTP accounting with gateway metadata and completion facts without changes to the core API; the access-log experiment of the first implementation (tag `prototype-final`) showed this.

Preserve tests distinguishing upstream HTTP errors from proxy execution errors and explicit rejections from unexpected failures. Test the failure-phase facts, including an unknown phase where the source is not known. Keep the access-log extension's result classification explicitly separate from the core execution model, and test optional response-writer instrumentation independently.

**Amended 2026-11-01:** [ADR-0013](0013-typed-rejections-and-replaceable-renderer.md) replaces the `HTTPError` experiment with typed rejections that carry headers, and a replaceable renderer with a `problem+json` default.

**Amended 2026-11-03:** [ADR-0018](0018-completion-observes-gateway-rejections.md) extends the completion guarantee to rejections by the secure defaults, and adds the rejection reason as an execution fact, including `no_route` for a routing miss. [ADR-0017](0017-failure-status-by-source.md) sets the status of a failure by its source.

**Note 2026-11-04:** Completion gives the first failure with its phase (`Failure`, `Phase`), with an unknown phase where the source is not known. This is the structured failure provenance that the decision calls for.

**Note 2026-11-04:** [ADR-0022](0022-rejections-and-the-problem-response.md) and [ADR-0023](0023-failure-evidence-and-transport-facts.md) implement the typed rejection, the renderer, and the failure evidence. The core never renders after the final response headers are committed.
