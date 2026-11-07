# ADR-0027: Check and forward request trailers at the end of the body

- **Status:** Accepted
- **Date:** 2026-11-05
- **Decision owners:** Project maintainers
- **Amends:** [ADR-0019](0019-hook-outputs-and-destination-ownership.md) (the request trailers), [ADR-0023](0023-failure-evidence-and-transport-facts.md) (the status precedence), and [ADR-0025](0025-core-protections-body-limit-admission-and-egress.md) (the request body)

## Context

The secure defaults check the trailer names that a request declares, and remove identity headers and managed headers from them (R3, R4). ADR-0019 says that the trailer values arrive while the body streams, after the transport started, so the core cannot check them.

Probes on Go 1.26 showed:

- At the end of an HTTP/1.1 chunked body, `net/http` merges each trailer field into the `Trailer` map of its request, also fields that the request did not declare, without a check of the name. When the map is nil, it makes a new map and sets the field of its own request, so a shallow copy of the request does not see it.
- An HTTP/2 server copies only the declared trailers, at the end of the body. It refuses forbidden fields with a stream error.
- `ReverseProxy` copies the trailer map of the request when it makes the outbound request, before the end of the body. The transports send the trailers from the map of the outbound request, after they read the body, on the goroutine that read it. Without a step of the core, no trailer value reaches the upstream.
- A request that shares the trailer map of the server shows middleware all names that arrive at the end of the body, also identity and managed names and names that reject mode refuses. A request with a copy of the map shows no values.

A loss of all values is not a protection: it hides missing checks.

## Decision

### Ownership

The request that the gateway gives to the global middleware always has its own trailer map. The map starts with the declared names that the secure defaults keep. For a request with a body, the map exists also when the request declared no trailers. The `Server` gives the request a trailer map before it makes its shallow copy, so the map that `net/http` fills is the map that the gateway reads.

### Check at the end of the body

The body wrapper of the request (ADR-0025) does the check once, when a read of the body returns `io.EOF` after the body limit, also with data in the same call. It reads the trailer map that `net/http` filled, on the goroutine that read the end of the body. A read that fails, that is truncated, or that goes over the body limit publishes no trailers. `Close` does not read the body to find trailers.

For each field, declared or not:

- A name with characters other than ASCII letters, digits, and `-` (R3): in reject mode, the read returns an error that wraps `ErrInvalidTrailerName` instead of `io.EOF`, and no trailer is published. In drop mode, the field is removed.
- Identity headers and managed headers (R4) are removed.
- Fields that a trailer must not contain (RFC 9110, section 6.5.1) are removed: the hop-by-hop fields, `Alt-Svc`, framing, `Host`, authentication fields with the cookies, request modifiers including each `If-` field, and content-format fields. The list starts from the fields that `net/http` refuses in HTTP/2 trailers; the `Gateway` documentation gives it. An upstream that merges trailers into the header section must not get such a value from a client after the middleware ran.
- Other fields are kept. A field is not refused only because the request did not declare it.

### Publication

The checked trailers go to the trailer map of the request that the gateway gave to the global middleware, and to the trailer map of the request that the transport gets. The core registers that map before the transport reads the body, and only when that request still has the body of the gateway and a trailer map. Middleware owns the outbound request: when it replaced the body or set the trailer map to nil, the transport gets no trailers from the client. Each map gets its own copy of the values. A value that middleware set is not replaced, and a declared name that middleware deleted from the outbound request is not added back. Names are compared in all letter cases, because middleware can set a trailer under a name that is not canonical; a client value then never goes next to the value of the middleware. A name that the request did not declare is added, because middleware cannot have seen it. Both writes run on the goroutine that read the end of the body. Code that reads the trailers must obey the rule of `net/http`: read them only after the read that returned `io.EOF`, on that goroutine or after a synchronization with it.

Middleware that reads the body to the end and passes on a copy of its request forwards the checked trailers in the copy. The transport sends the trailers that its protocol can send: HTTP/1.1 sends them with a chunked body, and an HTTP/2 upstream server keeps only the declared names.

When the upstream responds before the end of the body, the transport can continue to read the body, for example to a full-duplex upstream. The core then checks and forwards the trailers as above. A name that reject mode refuses then ends the upload, but the response that the gateway already sent does not change.

### Status

A late failure is not a rejection before the middleware. Completion keeps the facts that handling established, and records a `Failure` with the original error. The precedence of ADR-0023, as ADR-0025 amends it, gets a rule after the 413 rule and before the 504 rule:

- **Invalid trailer name (400).** The error wraps `ErrInvalidTrailerName`, and the trailer check of this request rejected a name. The 499 rule comes first. The core never renders after the final response headers are committed: when the upstream responded before the end of the body, the response continues or aborts, and no trailer is published.

## Consequences

**Positive:** Allowed trailer values reach middleware and the upstream. The trailer protections do not depend on a loss of data. Reject mode applies to all trailers, also to late and undeclared ones.

**Negative:** A request with a body costs one more map. Reject mode can fail a request at the end of a streamed body, after the upstream received the body. After an early response, a refused name ends the upload without a status that the client sees. Middleware that wraps the body of the gateway forwards no client trailers.

## Alternatives considered

- **A new empty trailer map for each request:** It fails closed, but middleware and the upstream never get trailer values.
- **Document the loss:** It keeps a protection that depends on a defect of the proxy.
- **Drop invalid late names also in reject mode:** It contradicts reject mode.
- **Publish to the outbound request also after middleware replaced the body:** It overrides the decision of the middleware, which owns the outbound request (ADR-0019).
- **Refuse all undeclared trailers:** HTTP/1.1 permits them.

## Validation / follow-up

Tests through the `Server` and the real transport verify the values at middleware and at the upstream on HTTP/1.1 and HTTP/2, with and without stripped headers, declared and undeclared fields, drop and reject mode, malformed and truncated bodies, a body over the limit, `Close` without a read, data with `io.EOF`, the 400 rule and its precedence, an early response in drop and reject mode, middleware that replaces the body, sets the trailer map to nil, deletes a declared trailer, or sets a trailer value, and no rendering after a commit.
