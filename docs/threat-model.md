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
| T1 | **Request smuggling or parsing differences:** the gateway and the upstream read a different request. | Use the `net/http` parser and `ReverseProxy` ([ADR-0002](adr/0002-use-go-standard-http-proxy.md)). Reject opaque URIs, absolute-form targets, and `CONNECT`. Remove hop-by-hop headers in both directions. | Done. The `Gateway` GoDoc in [gateway.go](../gateway.go) documents the deviation from RFC 9112. |
| T2 | **Path confusion:** encoded dot-segments let a request reach a path outside its route. | Path policy before routing, reject mode by default (R1). Normalize mode forwards the normalized path. | Reject mode is done. Normalize mode is planned for slice 1b. |
| T3 | **Query confusion:** semicolons make the gateway drop query parameters silently. | Reject query semicolons by default (R2). | Done. The gateway rejects each query that `url.ParseQuery` rejects, also for malformed percent-escapes and more than 10000 parameters, and forwards the raw query unchanged. The `Gateway` GoDoc documents the deviation. |
| T4 | **Header spoofing:** a client sets forwarding, identity, or extension-owned headers that the upstream believes. | Rebuild forwarding headers from the direct peer. Reject header-name aliases (R3). Remove untrusted identity headers and managed headers before extensions run (R4). Trusted proxies are a configurable protection (R6). | Forwarding headers, identity headers, managed headers, and aliases are done, also for the names of request trailers. The outbound header check applies R3 to the headers and trailer names that middleware and hooks set, for each request ([ADR-0019](adr/0019-hook-outputs-and-destination-ownership.md)). The embedder lists managed headers in the configuration, and extensions declare theirs with `HeaderOwner` ([ADR-0012](adr/0012-secure-defaults-and-configurable-protections.md)). The core removes both in the same step, also from trailer names and `Connection` tokens. The `Connection` step prevents the attack of Traefik CVE-2024-45410, where a client named forwarding headers in `Connection` so that the proxy removed them. R6 is not done. The `Gateway` GoDoc documents the deviation from RFC 9110. |
| T5 | **SSRF and egress bypass:** the gateway connects to a destination that the configuration does not allow. | Egress origin and network allowlists as a configurable protection. Check every DNS answer and dial the checked IP. No ambient environment proxy. | Opt-in [`extensions/egress`](../extensions/egress/README.md). The default transport does not use the environment proxy. |
| T6 | **Inbound resource exhaustion:** slow or large requests use all connections, memory, or capacity. | Header budgets and HTTP/2 limits (R8), progress-based idle deadlines (R9), also for upgraded connections, and a request-body limit as secure defaults. Admission capacity as a configurable protection. Stream bodies; do not buffer them. | Opt-in [`bodylimit`](../extensions/bodylimit/README.md) and [`admission`](../extensions/admission/README.md). The core has no server settings until slice 4. No idle deadlines. Upgraded connections (101) are hijacked: no server timeout applies to them, and server shutdown does not close them. |
| T7 | **Hostile or broken upstream:** large headers, hangs, compression bombs, or protocol hints that are wrong for the gateway's origin. | Transport timeouts and response-header limits. No transparent transport compression. Remove `Alt-Svc` (R7). | Done. `NewTransport` is the default transport. `Alt-Svc` and hop-by-hop fields are also removed from informational responses and trailers. Informational responses do not get the response headers that middleware set, and the final response keeps them. |
| T8 | **Sensitive data in responses or logs.** | One renderer writes minimal `problem+json` bodies with no failure details ([ADR-0013](adr/0013-typed-rejections-and-replaceable-renderer.md)). Diagnostics go through `internal/safelog` as source-only events. | Plain-text error bodies with the status text only, and `safelog`. No renderer until slice 3. See the [diagnostic audit](investigations/sensitive-diagnostics.md). |
| T9 | **Wrong execution facts:** a client cancellation or timeout is recorded as an upstream failure. This corrupts metrics and quota refunds. | Map errors to 504 and 499 (R5) from typed dial and request-written failures (R10), not from error text. | The source of a failure sets the status ([ADR-0017](adr/0017-failure-status-by-source.md)): 500 for hooks and selectors, 502 for the round trip. Completion gets the failure with its phase. Timeouts and cancellations are still 502 and are not typed: 504, 499, and R10 come in slice 3. |
| T10 | **Extension mistakes:** an extension leaks secrets, keeps request state too long, or adds unsafe headers back. | Extensions use the public contract only. Per-request state is serial and private ([ADR-0005](adr/0005-per-request-gateway-context.md)). Documentation states that extensions are trusted code. An extension that reads a response from another service, such as an authentication server, limits the size and the time of the read (compare Traefik CVE-2026-26998, an unbounded read of the ForwardAuth response). | Lifecycle hooks and typed request state are done. Request state is per request, and the core drops it at completion. Hooks cannot change the destination: `BeforeUpstream` gets a view whose only mutable part is the header map, and the selector contract accepts only upstream hosts that the transport sends without changes, so a signature over `Host` agrees with the wire. After the hooks, the core restores the outbound hop-by-hop fields, removes forbidden header-map entries from the outbound request and the response, and fails with 500 for an invalid outbound header name or value, also without hooks, for entries that middleware set ([ADR-0019](adr/0019-hook-outputs-and-destination-ownership.md)). Completion of a secure-default rejection gets no request content ([ADR-0018](adr/0018-completion-observes-gateway-rejections.md)). `Failure.Err` is the unchanged error and can contain secrets: an extension must not log it without its own policy. A hook can still set any header value; the core cannot check what the value means. |
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

- **TLS termination** is not decided yet ([ADR-0016](adr/0016-core-owns-the-inbound-server.md)).
- **Volumetric attacks** at the network level need protection in front of the
  gateway.
- **Network egress controls** outside the process are still necessary. The
  egress allowlist is not a complete SSRF defense.
- **Upstream authorization** stays the upstream's responsibility. The gateway
  can add checks, but it does not replace them.
- **A malicious extension** is out of scope. Extensions are trusted code.
- **A bare `http.Handler`** in another server does not get the server-level
  protections of T6 ([ADR-0016](adr/0016-core-owns-the-inbound-server.md)).
- **A deliberate relaxation** of a secure default moves the risk to the
  embedder ([ADR-0012](adr/0012-secure-defaults-and-configurable-protections.md)).

## Maintenance

Update this document when an ADR adds or changes a protection, when the rewrite
closes a gap in the _Current code_ column, or when a report shows a new threat.
