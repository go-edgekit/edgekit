# ADR-0018: Observe gateway-owned rejections at completion

- **Status:** Accepted
- **Date:** 2026-11-03
- **Decision owners:** Project maintainers
- **Amends:** [ADR-0004](0004-compose-http-middleware-and-hooks.md) (ordering) and [ADR-0009](0009-outcomes-errors-and-early-responses.md) (completion guarantee)

## Context

ADR-0004 starts gateway-managed handling with the gateway context and the completion observer. ADR-0012 later added secure defaults that reject a request before any middleware runs. Neither ADR says whether completion observes these rejections. If it does not, the requests that the gateway itself refuses, such as path-confusion attempts, are not visible to access logs or metrics. Response-observing middleware cannot see them either, because no middleware runs.

Caddy installs its access-log observer before its server-level checks, and Envoy records reasons such as `path_normalization_failed` in its connection manager. Both keep observation outside the checks.

A routing miss has a similar problem. Without a fact, completion cannot tell a routing miss from middleware that ends handling before routing: in both, no route is matched and no failure occurs.

Rejections divide into three kinds:

1. A rejection before an HTTP request reaches gateway handling, for example by the server. Gateway completion cannot observe it.
2. A rejection by the gateway's own request checks or routing.
3. A rejection by middleware or hooks.

## Decision

Completion observes the second kind. The ordering of ADR-0004 becomes:

```text
Initialize gateway request context and install completion observer
  Secure-default request checks ── rejection → completion
  Remove identity headers and managed headers
  Global HTTP middleware: before
    RequestReceived hooks
    Match route ── no route → rejection, completion after middleware unwinds
    ... (unchanged from ADR-0004)
  Global HTTP middleware: after
RequestCompleted hooks in the outer completion observer
```

### Rejection reason

Completion gets a **rejection reason**, a public code that names the core check that refused the request. Its contract:

- Only the core sets it, and only when the core rejects a request.
- Its values come only from exported constants of the core. A named string type gives the codes one documented place. The type does not enforce membership, so the core must make sure that each value it records obeys this contract.
- Empty means that no explicit rejection was recorded. It does not mean that the request was accepted.
- The core never infers it from an HTTP status that it observed.
- It names the check, never the value that failed the check. It contains no request content and no diagnostic text.

The core codes are `invalid_method`, `invalid_request_target`, `invalid_path`, `invalid_query`, `invalid_header_name`, and `no_route`. The field and its string type are stable: the typed rejection of [ADR-0013](0013-typed-rejections-and-replaceable-renderer.md) supplies a reason code of the same type, and does not replace the field. Add other completion facts only when a requirement needs them.

### Facts and data restrictions

The facts depend on the stage where the rejection occurs:

- **Secure-default rejection.** No middleware and no processing hooks (`RequestReceived`, `BeforeUpstream`, `UpstreamResponse`) run. Completion hooks run. The core passes:
  - the entry context, without changes except for the gateway context value;
  - the gateway context, with only the start time: no route, no selected upstream, and no request state, because no extension ran;
  - the completion facts: start time, duration, and the rejection reason. The other facts are empty.
- **Routing rejection (`no_route`).** Global middleware and `RequestReceived` hooks can have run already. The core keeps the facts and the request state that they established. Only the later phases do not run.

For a secure-default rejection, the core does not pass or keep the method, the request target, the path, the query, header or trailer names or values, the body, or the peer address. The completion hook signature has no request, but that alone does not prove that no sensitive value is reachable. The entry context belongs to the caller: it can hold values that the server or outer wrappers added, such as `http.ServerContextKey`, the local address, or values of outer middleware. The core adds no value that it derived from the request, and it makes no claim about values that others added.

If middleware does not derive its context from the request context, inner stages cannot find the gateway context. The core then fails closed with 500 Internal Server Error and does not proxy. Completion still runs, because the outer observer holds the gateway context, but it can only report the facts that were recorded before that point.

ADR-0009's completion guarantee includes gateway-owned rejections: completion runs once after the secure-default checks reject a request, or after the gateway-managed handler chain returns or unwinds.

## Consequences

**Positive:** Access logs and metrics see attacks that the secure defaults stop. A routing miss is an explicit execution fact, not an interpretation of a 404.

**Negative:** Completion hooks run for requests that no other extension saw, so they must not assume that middleware state exists. The reason code becomes public API.

## Alternatives considered

- **No completion for secure-default rejections:** It keeps "before any extension runs" literal, but it leaves an observation gap that no extension can close.
- **Pass the rejected request to completion:** It gives more data to access logs, but it exposes unsanitized headers and request content that the secure defaults just refused.
- **A full rejection value in completion:** The rejection type of ADR-0013 carries a status and response headers, which are HTTP observations, not execution facts (ADR-0009). A completion field of that type would also change each time the rejection type changes.

## Validation / follow-up

Tests verify the reason code for each secure-default check, that no middleware and no processing hook runs, and that the gateway context has only the start time. Tests verify `no_route` together with the state that global middleware set, and a missing trailing slash as a routing miss.

**Amended 2026-11-04:** [ADR-0022](0022-rejections-and-the-problem-response.md) lets a hook supply the reason through a valid rejection, with a code of the form `<extension>.<check>`; the codes of the core never contain `.`. [ADR-0023](0023-failure-evidence-and-transport-facts.md) adds the core code `invalid_upgrade`.

**Amended 2026-11-05:** [ADR-0025](0025-core-protections-body-limit-admission-and-egress.md) adds the gateway admission, the route body check, and the route admission to the handling order, and the core codes `body_too_large` and `admission_saturated`.
