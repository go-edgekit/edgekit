# ADR-0025: Core protections: body limit, admission, and egress

- **Status:** Accepted; body-limit replacement, handling order, and admission coverage superseded in part by [ADR-0031](0031-route-first-request-bodies.md)
- **Date:** 2026-11-05
- **Decision owners:** Project maintainers
- **Refines:** [ADR-0012](0012-secure-defaults-and-configurable-protections.md) (body limit, admission, and egress)
- **Amends:** [ADR-0018](0018-completion-observes-gateway-rejections.md) (the handling order and the core reason codes) and [ADR-0023](0023-failure-evidence-and-transport-facts.md) (the status precedence and R10)

## Context

ADR-0012 makes the request-body limit a secure default with a conservative value, and admission capacity and the egress allowlists configurable protections. It does not give the value of the body limit, how a route overrides it, how a caller disables it, where admission and the body checks run in the handling order, how they are observed, or how the core reports an egress denial. ADR-0023 says that a typed dial failure for the failed attempt needs a dialer that the core owns.

Facts that the decision uses:

- A body limit that applies only to declared lengths is not a limit: HTTP/1.1 chunked bodies and HTTP/2 bodies can have no declared length. A limit that applies only while the body streams cannot be an atomic rejection: the upstream can receive a part of the body, and it can respond before the limit is reached.
- Global middleware and `RequestReceived` hooks run before routing, so a route limit is not known when they can read the body.
- On Go 1.26, `http.Transport` returns an error from the request-body reader so that `errors.Is` finds it, on HTTP/1.1 and HTTP/2.
- `http.Transport` dials with a context that it detaches from the cancellation of the request, because another request can use the connection. When the request is canceled, it returns the cancellation error, and the dial continues. A dial error goes to the request that waits for that dial.
- A DNS answer can change between a check and a connection (DNS rebinding). `http.Transport` has paths that do not use `DialContext`: a proxy, `DialTLSContext`, `TLSNextProto`, and protocols that `RegisterProtocol` adds.

## Decision

### Handling order

The order of ADR-0018 becomes:

```text
Initialize gateway request context and install completion observer
  Secure-default request checks ── rejection → completion
  Remove identity headers and managed headers; limit the body with the gateway limit
  Global HTTP middleware: before
    Gateway admission ── saturated → 503 admission_saturated
    RequestReceived hooks
    Match route ── no route → 404 no_route
    Route body check ── over the route limit → 413 body_too_large
    Route admission ── saturated → 503 admission_saturated
    Route HTTP middleware, upstream selection, BeforeUpstream hooks, transport ...
    (admission slots are released when this part returns or unwinds)
  Global HTTP middleware: after
RequestCompleted hooks in the outer completion observer
```

### Body limit

The body limit is a secure default. `Config.BodyLimit` sets the gateway limit and `Route.BodyLimit` sets the limit of a route. The value type `BodyLimit` keeps three states apart:

- The zero value: omitted. For the gateway, it is the default limit. For a route, it is the gateway limit.
- `MaxBodyBytes(n)`: at most n bytes. `MaxBodyBytes(0)` permits only empty bodies. A negative n is a configuration error.
- `NoBodyLimit()`: no limit. This is the explicit relaxation of ADR-0012.

The default is 10 MiB (10,485,760 bytes). It is the initial default for ordinary API requests, not an industry standard and not a value that is safe for all workloads; other gateways use different values and different enforcement models. Upload routes and other routes with large bodies set an explicit limit. A later release can change the default, with a stated reason and the effect on compatibility. A caller that needs an exact limit sets it explicitly. A gateway never changes its limits after `New`.

One wrapper limits the bytes that readers get from the body. It does not buffer and does not read ahead: it reads at most one byte more than the remaining limit, to find an overflow at the exact limit. It does not wrap the response writer. `Close` goes to the original body, and a nil body or `http.NoBody` stays unchanged.

- Before routing, the gateway limit applies to the readers.
- At the route match, the route limit replaces the gateway limit. The bytes that were read before still count.
- An overflow is final: a larger route limit cannot undo an overflow of the gateway limit.
- At the route match, before the route middleware and the forwarding, the core rejects the request with 413 and the reason `body_too_large` when the limit had an overflow, when the bytes that were read are more than the route limit, or when the declared length is more than the route limit. This is a rejection: completion gets the reason and the matched route, and no failure.

The declared-length check does not prevent all body reads and all `100 Continue` responses: global middleware or a `RequestReceived` hook can read the body before routing. The core does not route early to make the guarantee stronger.

An overflow while a reader streams the body is a failure, not a rejection. The reader gets an error that wraps `ErrRequestBodyTooLarge` and an `*http.MaxBytesError`. Completion gets the original error in `Failure.Err`. When the gateway can still send a response, the status is 413 (see the status precedence). The core never renders after the final response headers are committed or after a hijack: when a reader of the response body, such as an upstream that echoes the body, gets the overflow while the response streams, the response aborts as other streaming failures do (ADR-0009). The streaming limit is not an atomic rejection: the upstream can receive a part of the body before the overflow, and an upstream that responds before the overflow can finish the exchange without a failure.

### Admission

Admission capacity is a configurable protection. A caller makes an `*AdmissionBudget` with `NewAdmissionBudget(limit)`, where the limit is positive, and sets it in `Config.Admission`, in `Route.Admission`, or both. Nil means no admission. The budget holds the runtime state; the configuration only refers to it. Routes that refer to the same budget share it. A budget can also be shared between gateways. `New` rejects a budget that is both the gateway budget and a route budget, because one request would then take two slots of it, and a budget that `NewAdmissionBudget` did not make.

- The gateway budget is acquired after the global middleware and before the `RequestReceived` hooks, so middleware that observes responses sees a 503 from saturation. A route budget is acquired after the route body check and before the route middleware.
- There is no queue. When the budget has no free slot, the core rejects the request with 503 and the reason `admission_saturated`, without a `Retry-After` header. The admission step does not read or close the request body.
- A slot is held until the protected part returns or unwinds: also while the body streams, while the transport waits for a pooled connection, and while an upgraded connection is active. A cancellation alone does not release a slot. The release runs on a normal return and during a panic, and the panic continues.
- Each request counts, not each connection, so the streams of one HTTP/2 connection share a budget.
- Admission limits concurrency. It is not a rate limit and contains no business policy.

Coverage of each budget:

- The gateway budget does not cover the secure-default checks, the global middleware, the rendering of its own saturation response, completion, and the work of the server, such as the parsing of headers and the drain of an unread body after the handler returns.
- A route budget also does not cover the `RequestReceived` hooks, the routing, the route body check, and the rendering of its own saturation response. A route saturation response is rendered while the request holds a gateway slot, if it has one.
- A connection that middleware hijacks is outside admission after its handler returns.

### Egress

The egress allowlists are a configurable protection. `NewEgressTransport(template, policy)` returns an `*EgressTransport`, which a caller sets in `Config.Transport`. The configuration has no other egress field. The transport type is opaque, so its settings cannot change after construction.

- The `EgressPolicy` has the allowed HTTP(S) origins and IP networks, a `net.Dialer` for the connection settings, and an `EgressResolver`. The core copies the lists and the dialer. Both lists must not be empty. There are no implicit permissions or prohibitions for private, loopback, or public networks. Unspecified, multicast, and scoped addresses are always denied.
- The transport checks the origin of each request before it gives the request to the connection pool, so a request on a pooled connection is also checked. A denied request has its body closed without a read.
- For each new connection, the dialer resolves the host once, checks every address in the answer before any connection attempt, and denies an empty, invalid, or mixed allowed and denied answer. It dials only the checked IP literals, one after the other, and never resolves the host again. One deadline, the dialer timeout from the start of the dial, limits the resolution and all attempts. The `Host` header, the TLS server name, and the certificate verification use the request host.
- The transport is a new `http.Transport` with its own connection pool. It copies the timeouts, the pool limits, the buffer sizes, the response-header limit, the HTTP/2 settings, and a clone of the TLS configuration from the template. The egress dialer replaces `DialContext`. `NewEgressTransport` rejects a template with a proxy, `DialTLSContext`, a non-empty `TLSNextProto`, proxy-connect settings, a TLS `ServerName`, or transparent compression. It does not copy the deprecated `Dial` and `DialTLS` fields: the lint rules of the project forbid a use of deprecated fields, also to examine them, and the new transport never calls a field that it did not copy. It cannot see protocols that `RegisterProtocol` added, so it does not copy them. The documentation of `NewEgressTransport` lists the fields that it replaces and does not copy. A nil template uses the settings of `NewTransport`.
- A denial is a failure in `PhaseRoundTrip` with an error that wraps `ErrEgressDenied`, and gets 502 as a failure of the round trip (ADR-0017). This applies to origin and address denials. The error contains no URL and no address.

### Typed dial failure (R10)

The egress dialer returns a `*DialError` for each failed dial. It is evidence that the transport could not establish a connection, and that the transport gave this error to the round trip. It is not proof that this request started the dial: `http.Transport` decouples the life of a dial from the request.

- `Attempts` lists the connection attempts of this one dial call, in order, each with its address and its error. It is empty when no attempt was made, such as after a lookup failure or a denial. It does not describe other dials of the round trip.
- `Err` is the cause: the lookup error, `ErrEgressDenied`, the context error when the dial context ended, or the error of the last attempt. `Unwrap` returns it.
- The dialer makes a new `DialError` for each failure and does not change it after it returns it. The value and its slice are safe for concurrent reads. Callers must not change them.
- `DialError` does not implement `net.Error`, so the 504 and 499 rules see the error that it wraps.
- `ConnectionObtained` and `RequestWritten` stay facts about the request (ADR-0023).

The default transport from `NewTransport` does not return a `DialError`. R10 is complete only for the egress transport.

### Status precedence

The precedence of ADR-0023 gets a new rule after the client-gone rule (499) and before the upstream-timeout rule (504):

- **Request body too large (413).** The error wraps `ErrRequestBodyTooLarge`, and the body limit of this request recorded an overflow. This applies in each processing stage. One of the two alone does not change the status: an error from another source that wraps the sentinel gets the status of its source.

The body limit records the overflow in its own synchronized value, not in the gateway context, because a transport can read the body on another goroutine.

### Reason codes

The core codes of ADR-0018 get `body_too_large` (413) and `admission_saturated` (503).

## Consequences

**Positive:** A gateway without configuration limits request bodies. Body limits, admission, and egress have one public contract and one place in the handling order. Completion observes the rejections of the protections with a reason. A body overflow, a dial failure, and an egress denial are typed execution facts, so the core never reads error text.

**Negative:** Requests with bodies over 10 MiB fail until the route sets a larger limit. The streaming limit can leave a partial upload at the upstream. Global middleware and `RequestReceived` hooks are outside the gateway budget. The egress dialer tries addresses one after the other, without Happy Eyeballs, so a slow first address uses the deadline. A `DialError` is available only with the egress transport.

## Alternatives considered

- **An `int64` limit with negative sentinels:** It needs one sentinel for "no limit" and one for "empty bodies only", because zero means the default.
- **The body checks before the global middleware:** The route limit is not known there, and early routing would duplicate the router.
- **A streaming overflow as a rejection:** Completion would lose the original error, and the classification would differ from other failures of the round trip.
- **413 from the sentinel alone:** A hook that reads an upstream response with `http.MaxBytesReader`, or an unrelated error, would get 413.
- **Gateway admission before the global middleware:** It covers more work, but middleware that observes responses does not see the 503.
- **Admission limits as integers in the configuration:** Sharing a budget between routes then needs names, and the runtime state has no explicit owner.
- **An egress field in the gateway configuration:** The gateway would own a transport that is built from a caller's template, with two meanings for `Config.Transport`.
- **Silently ignoring the bypass fields of the template:** A caller would not see that a proxy or a custom TLS dialer is not used.
- **500 for an origin denial:** It separates selector defects from DNS answers, but needs a second sentinel and a new precedence rule.

## Validation / follow-up

Tests verify the exact limit, unknown and declared lengths, the stages and the overflow rules at the route match, the 413 rule with and without evidence and its place in the precedence, no rendering after a commit, saturation without a queue or a body read, release on return and panic, cancellation, pool waits, streaming, HTTP/1.1 and HTTP/2, shared and separate budgets, origin and address classification, mixed and changed DNS answers, the dial of literals only, fallback in one deadline, the TLS identity, pool reuse, the rejected bypass fields, and the 499 and 504 rules through `DialError`.

A typed dial failure for `NewTransport` needs its own decision. Happy Eyeballs for the egress dialer needs workload evidence.

**Amended 2026-11-05:** [ADR-0027](0027-request-trailers-at-the-end-of-the-body.md) makes the body wrapper also check and publish the request trailers at a successful end of the body, so each request with a body gets the wrapper, also without a limit.
