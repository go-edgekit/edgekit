# Threat model

**Status:** Describes the target design from the [ADRs](adr/README.md). The
current code does not have all of the listed protections yet. The _Current
code_ column shows the gaps. This document does not claim production
readiness.

## Scope

edgekit is a Go library. An embedder compiles the core and the chosen
extensions into one gateway binary ([ADR-0006](adr/0006-compile-time-go-extensions.md),
[ADR-0011](adr/0011-library-first-gateway-framework.md)). The gateway receives
HTTP requests, matches routes, and proxies them to upstreams.

This model covers the core, the bundled extensions, and the project's supply
chain. It does not cover a distribution, the deployment network, or the
upstream application.

## Assets

- **Upstream integrity:** the upstream receives the request that the gateway
  matched and checked, with no spoofed identity or forwarding data.
- **Network reach:** the gateway connects only to the destinations that the
  configuration allows.
- **Availability:** one client cannot use all connections, memory, or
  admission capacity.
- **Secrets:** API keys, credentials, request content, and errors do not reach
  responses or logs.
- **Execution facts:** the core records what happened correctly. Logs, metrics,
  and quota refunds depend on them.
- **Policy data:** key hashes, customers, and limit counters, in the bundled
  access-policy extensions.

## Actors and trust

| Actor | Trust | Reason |
| --- | --- | --- |
| Client | Untrusted | Controls the request line, headers, body, and timing. |
| Proxy in front of the gateway | Untrusted, unless the configuration lists it as a trusted proxy (R6) | Can be a client that sends forwarding headers. |
| Upstream | Partly trusted | Owns its response content, but can be slow, broken, or hostile toward the gateway. |
| DNS | Untrusted for destination decisions | Answers can change between check and connect. |
| Embedder and configuration | Trusted | Decides routes, protections, and destinations. A deliberate relaxation is the embedder's choice. |
| Extension | Trusted code, not a sandbox | Runs in the same process with the public contract only. |
| Log reader | Trusted with limited data | Must not see secrets or request content. |
| Dependency and CI action | Partly trusted | Pinned and checked, but a compromise is possible. |

## Trust boundaries

1. **Network to server:** the core-owned server parses the connection
   ([ADR-0016](adr/0016-core-owns-the-inbound-server.md)). Server-level limits
   apply here, before routing.
2. **Server to handling:** the core applies the request secure defaults before
   any extension runs.
3. **Core to extension:** extensions get the public contract only. They can
   reject a request, but they cannot remove core protections.
4. **Gateway to upstream:** the transport connects to the selected upstream.
   Egress checks apply here.
5. **Gateway to sinks:** responses and logs leave the process. Only safe data
   crosses this boundary.

## Threats and mitigations

R numbers refer to the recommendations in the
[proxy implementation survey](investigations/proxy-implementation-survey.md#recommendations-for-this-project).

| ID | Threat | Target mitigation | Current code |
| --- | --- | --- | --- |
| T1 | **Request smuggling or parsing differences:** the gateway and the upstream read a different request. | Use the `net/http` parser and `ReverseProxy` ([ADR-0002](adr/0002-use-go-standard-http-proxy.md)). Reject opaque URIs, absolute-form targets, and `CONNECT`. Remove hop-by-hop headers in both directions. | Done. The `Gateway` GoDoc in [gateway.go](../gateway.go) documents the deviation from RFC 9112. A client `Upgrade` value that is not printable ASCII, with the `upgrade` token in `Connection`, is rejected with 400 (`invalid_upgrade`), because `ReverseProxy` would refuse it without a source ([ADR-0023](adr/0023-failure-evidence-and-transport-facts.md)). `Config.AllowAbsoluteForm` accepts an HTTP(S) absolute-form target without user information or fragment, with a host that the selector contract accepts. The gateway then handles the origin-form request with `Host` set to the authority (RFC 9112, section 3.2.2). The authority goes to the upstream only in `X-Forwarded-Host`, and does not select the destination, except through an upstream selector that reads `Host`, as for the `Host` header of an origin-form request; an egress transport still checks the selected destination ([ADR-0026](adr/0026-relax-request-secure-defaults.md)). An unescaped `#` anywhere in the target, in both forms, is a fragment and is rejected with 400 (`invalid_request_target`), as Envoy and HAProxy (CVE-2023-45539) do; `url.ParseRequestURI` keeps it in the path or the raw query, so the core checks the raw target. `CONNECT`, the other target forms, the fragment check, and the `Upgrade` check cannot be relaxed. |
| T2 | **Path confusion:** encoded dot-segments let a request reach a path outside its route. | Path policy before routing, reject mode by default (R1). Normalize mode forwards the normalized path. | Reject mode is done. Normalize mode is planned for slice 1b. |
| T3 | **Query confusion:** semicolons make the gateway drop query parameters silently. | Reject query semicolons by default (R2). | Done. The gateway rejects each query that `url.ParseQuery` rejects, also for malformed percent-escapes and more than 10000 parameters, and forwards the raw query unchanged. The `Gateway` GoDoc documents the deviation. `QueryUnchecked` relaxes the check for the complete gateway; middleware that parses the query can then see other parameters than the upstream ([ADR-0026](adr/0026-relax-request-secure-defaults.md)). |
| T4 | **Header spoofing:** a client sets forwarding, identity, or extension-owned headers that the upstream believes. | Rebuild forwarding headers from the direct peer. Reject header-name aliases (R3). Remove untrusted identity headers and managed headers before extensions run (R4). Trusted proxies are a configurable protection (R6). | Forwarding headers, identity headers, managed headers, and aliases are done, also for request trailers ([ADR-0027](adr/0027-request-trailers-at-the-end-of-the-body.md)). The request that middleware gets has its own trailer map. At a successful end of the body, the core checks all trailers that arrived, also undeclared ones: an invalid name fails the read with `ErrInvalidTrailerName` in reject mode (400 when a response is still possible) or is removed in drop mode, and identity, managed, hop-by-hop (also names in the inbound `Connection` options), framing, `Host`, authentication, cookie, request-modifier (also `If-`), and content-format fields are removed (RFC 9110, sections 6.5.1 and 7.6.1). The core then publishes copies of the other values to the request of the middleware, and to the request that the transport gets while it has the body of the gateway and a trailer map, without replacing values that middleware set or adding back declared names that middleware deleted. So allowed trailers reach the upstream when its protocol can carry them, also when middleware copied its request before reading the body to EOF. A middleware value in any letter case takes precedence over a client value. Middleware that replaces the body or removes the trailer map forwards none. A failed, truncated, or oversized body publishes no trailers. On HTTP/1.1, undeclared trailers reach the gateway only through `Server`, or through a request that shares a trailer map with the request of `net/http`; middleware in front of the gateway that copies the request without a map loses them, so they reach neither middleware nor the upstream. After an early upstream response, a transport that continues to read the body still gets checked trailers, and a rejected name ends the upload without a change to the response. The outbound header check applies R3 to the headers and trailer names that middleware and hooks set, for each request ([ADR-0019](adr/0019-hook-outputs-and-destination-ownership.md)). The embedder lists managed headers in the configuration, and extensions declare theirs with `HeaderOwner` ([ADR-0012](adr/0012-secure-defaults-and-configurable-protections.md)). The core removes both in the same step, also from trailer names and `Connection` tokens. The `Connection` step prevents the attack of Traefik CVE-2024-45410, where a client named forwarding headers in `Connection` so that the proxy removed them. R6 is not done. The `Gateway` GoDoc documents the deviation from RFC 9110. `HeaderNameDrop` removes invalid header and trailer names before any middleware runs, instead of a rejection; the gateway never forwards such a name, and the identity-header removal and the outbound check stay in force ([ADR-0026](adr/0026-relax-request-secure-defaults.md)). |
| T5 | **SSRF and egress bypass:** the gateway connects to a destination that the configuration does not allow. | Egress origin and network allowlists as a configurable protection. Check every DNS answer and dial the checked IP. No ambient environment proxy. | Done ([ADR-0025](adr/0025-core-protections-body-limit-admission-and-egress.md)). `NewEgressTransport` makes a transport that checks the origin of each request before its connection pool, resolves each new connection once, checks every address of the answer before any attempt, denies empty, invalid, and mixed answers and unspecified, multicast, and scoped addresses, rejects allowed origins whose host ends in a number, such as `127.1`, so an address has only one spelling, and dials only the checked literals, one after the other, in one deadline. Host, SNI, and certificate verification use the request host. It rejects templates with a proxy, a custom TLS dialer, protocol callbacks, proxy-connect settings, or a TLS server name, and does not copy registered protocols. A denial is a 502 failure with `ErrEgressDenied` and no URL or address in the error. It is opt-in, because the allowed destinations depend on the deployment. The default transport does not use the environment proxy. Known limits: a connection in the pool can be reused after the DNS answer changed; DNS queries are not limited by the networks; there is no Happy Eyeballs, so a slow first address uses the deadline; an extension that sends its own requests does not use the gateway transport. |
| T6 | **Inbound resource exhaustion:** slow or large requests use all connections, memory, or capacity. | Header budgets and HTTP/2 limits (R8), progress-based idle deadlines (R9), also for upgraded connections, and a request-body limit as secure defaults. Admission capacity as a configurable protection. Stream bodies; do not buffer them. | Server limits done ([ADR-0016](adr/0016-core-owns-the-inbound-server.md), [ADR-0024](adr/0024-hardened-server-deadlines-and-upgraded-connections.md)). `Server` has a header-read timeout of 10s that also limits the TLS handshake, a header budget of 64 KiB with the accounting of `net/http`, and HTTP/2 limits of 100 streams, a 64 KiB stream window, and a 1 MiB connection window. Transfer idle deadlines (300s) apply to body reads, to response writes in parts of 16 KiB, and to the reads of an unread body that `net/http` does itself. On HTTP/2 they apply to one stream, and the connection has a separate write timeout. There are no total read or write timeouts. The upgraded connections of the gateway get the same idle deadlines below TLS, and `Shutdown` closes them before it waits for other requests. Known limits: the header-read timeout does not limit the headers of each HTTP/2 stream; a handler that sets its own deadline takes over that direction; a connection that middleware hijacks is not managed; a transfer slower than 16 KiB for each timeout stops. The body limit is done as a secure default of 10 MiB ([ADR-0025](adr/0025-core-protections-body-limit-admission-and-egress.md)), an initial value for ordinary API requests that routes with large bodies replace. It streams without read-ahead, uses the gateway limit before routing and the route limit after the route match, rejects a declared or consumed length over the route limit with 413 before the route middleware, and gives readers an error that wraps `ErrRequestBodyTooLarge` at the exact limit. A streaming overflow is not an atomic rejection: the upstream can get a part of the body, and middleware or `RequestReceived` hooks that read before routing can cause `100 Continue`. Admission is done as a configurable protection: `AdmissionBudget` values for the gateway (after the global middleware) and for routes, with no queue, a 503 without a body read, release on return and panic, and one budget for the streams of an HTTP/2 connection. The global middleware, completion, the rendering of a saturation response for the budget that is full, and the work of the server are outside a budget; a route saturation response is rendered while the request holds its gateway slot. A connection that middleware hijacks is outside admission after its handler returns. |
| T7 | **Hostile or broken upstream:** large headers, hangs, compression bombs, or protocol hints that are wrong for the gateway's origin. | Transport timeouts and response-header limits. No transparent transport compression. Remove `Alt-Svc` (R7). | Done. `NewTransport` is the default transport. `Alt-Svc` and hop-by-hop fields are also removed from informational responses and trailers. Informational responses do not get the response headers that middleware set, and the final response keeps them. The core closes the upstream body when an accepted protocol switch fails. The core and `ReverseProxy` share a body that closes the underlying stream at most once, also when their close calls are concurrent. |
| T8 | **Sensitive data in responses or logs.** | One renderer writes minimal `problem+json` bodies with no failure details ([ADR-0013](adr/0013-typed-rejections-and-replaceable-renderer.md)). Diagnostics go through `internal/safelog` as source-only events. | Done ([ADR-0022](adr/0022-rejections-and-the-problem-response.md)). The renderer gets only the status and the rejection reason, never the failure, so a replacement renderer cannot leak failure details. The body has no `detail` and no `instance`, so it never echoes the request path. For each response that it makes, the core sends exactly one `X-Content-Type-Options: nosniff` value. It replaces values from middleware and rejections in all letter cases, also with a custom renderer. A reason code from an extension reaches the client: its syntax (`<extension>.<check>`, lowercase letters, digits, and `_`, at most 64 bytes) limits what it can contain, but the extension must keep it a static name, never an identifier or a request value. The core never renders after the final headers are committed or after a hijack. Diagnostics go through `safelog`. The `Server` sends the messages of `net/http`, such as TLS handshake failures, parse failures, and recovered panics, as source-only events, and writes no startup or shutdown logs. See the [diagnostic audit](investigations/sensitive-diagnostics.md). |
| T9 | **Wrong execution facts:** a client cancellation or timeout is recorded as an upstream failure. This corrupts metrics and quota refunds. | Map errors to 504 and 499 (R5) from typed dial and request-written failures (R10), not from error text. | Done in part ([ADR-0017](adr/0017-failure-status-by-source.md), [ADR-0023](adr/0023-failure-evidence-and-transport-facts.md)). 499 needs both an error that wraps `context.Canceled` and a canceled client context, so a canceled context alone never hides another failure. 504 needs a typed timeout from the round trip. The core never reads error text. Completion gets the original failure with its phase, and the facts `ConnectionObtained` and `RequestWritten` from `httptrace`, read once when completion starts. These facts are for the request, not for the failed attempt, and false means "not reported". Only the transport gets the trace, so a request that a response hook sends cannot set the facts. The egress transport returns a `DialError` with the attempts of one failed dial ([ADR-0025](adr/0025-core-protections-body-limit-admission-and-egress.md)). It is evidence that the transport could not establish a connection, not proof that this request started the dial, and it does not change the 499 and 504 rules. `NewTransport` does not return it. A streaming body overflow gives 413 only when the body limit of the request found the overflow and the error wraps `ErrRequestBodyTooLarge`. A read error other than `io.EOF` from the final response body reaches completion in `PhaseResponseBody`, with the original error, also after the response is committed ([ADR-0028](adr/0028-response-body-failures-at-completion.md)). The observer runs on the response-copy goroutine and does not render or change response-write observations. Known limit: `net/http` returns the cancellation error when the request context is done, also when a dial failed at the same time. |
| T10 | **Extension mistakes:** an extension leaks secrets, keeps request state too long, or adds unsafe headers back. | Extensions use the public contract only. Per-request state is serial and private ([ADR-0005](adr/0005-per-request-gateway-context.md)). Documentation states that extensions are trusted code. An extension that reads a response from another service, such as an authentication server, limits the size and the time of the read (compare Traefik CVE-2026-26998, an unbounded read of the ForwardAuth response). | Lifecycle hooks and typed request state are done. Request state is per request, and the core drops it at completion. Hooks cannot change the destination: `BeforeUpstream` gets a view whose only mutable part is the header map, and the selector contract accepts only upstream hosts that the transport sends without changes, so a signature over `Host` agrees with the wire. After the hooks, the core restores the outbound hop-by-hop fields, removes forbidden header-map entries from the outbound request and the response, and fails with 500 for an invalid outbound header name or value, also without hooks, for entries that middleware set ([ADR-0019](adr/0019-hook-outputs-and-destination-ownership.md)). Completion of a secure-default rejection gets no request content ([ADR-0018](adr/0018-completion-observes-gateway-rejections.md)). `Failure.Err` is the unchanged error and can contain secrets: an extension must not log it without its own policy. A hook can still set any header value; the core cannot check what the value means. A hook can reject a request with its own status, headers, and reason code, as a result separate from its error ([ADR-0022](adr/0022-rejections-and-the-problem-response.md)): the core accepts only 400 to 599, checks the header names (R3) and values, removes hop-by-hop, framing, `Content-Type`, and `Alt-Svc` fields, and sends 500 for an invalid rejection, without its values in the failure or in diagnostics. An error always wins over a rejection, so a rejection cannot hide a failure. The core copies the header map and never changes a shared rejection value. The type does not guarantee the decision: an extension can still return a rejection where it should return a failure. Middleware must not commit a response before it calls `next`, because the core cannot see such a commit. |
| T11 | **Supply-chain compromise** of a dependency, a CI action, or the toolchain. | Keep the core dependency-light ([ADR-0014](adr/0014-multi-module-repository.md)). Pin actions by commit. Run zizmor, govulncheck, and CodeQL. Dependabot updates wait 7 days. Protect `main`. | Same. |

### Access-policy extensions

These threats apply to the bundled extensions in
[ADR-0015](adr/0015-access-policy-model.md), not to the core.

| ID | Threat | Mitigation |
| --- | --- | --- |
| P1 | **API key theft from storage or logs.** | Stores keep key hashes only. Subjects and analytics use the key ID, never the secret. Keys come from a header; query-parameter keys are off by default. |
| P2 | **Key enumeration:** different responses show which keys exist. | Missing and unknown keys get the same 401 rejection. |
| P3 | **Quota drain by a sibling key:** one key uses a customer's shared quota with denied requests. | A request consumes from its limits only when all limits allow it. |
| P4 | **Quota abuse through refunds:** a client causes failures that refund quota. | A refund uses the execution facts of T9. Analytics records are never a source for quota counts. |

## Assumptions and out of scope

- **TLS termination** is not decided yet ([ADR-0016](adr/0016-core-owns-the-inbound-server.md)). `Server` accepts a `tls.Config` from the embedder, who owns the certificates and the TLS settings.
- **Volumetric attacks** at the network level need protection in front of the
  gateway.
- **Network egress controls** outside the process are still necessary. The
  egress allowlist is not a complete SSRF defense: NAT, proxies outside the
  process, service meshes, and upstream redirects that a client follows are
  out of its reach.
- **Upstream authorization** stays the upstream's responsibility. The gateway
  can add checks, but it does not replace them.
- **A malicious extension** is out of scope. Extensions are trusted code.
- **A bare `http.Handler`** in another server does not get the server-level
  protections of T6 ([ADR-0016](adr/0016-core-owns-the-inbound-server.md)).
  `Server` is the supported way to serve a gateway.
- **A deliberate relaxation** of a secure default moves the risk to the
  embedder ([ADR-0012](adr/0012-secure-defaults-and-configurable-protections.md)).

## Maintenance

Update this document when an ADR adds or changes a protection, when the rewrite
closes a gap in the _Current code_ column, or when a report shows a new threat.
