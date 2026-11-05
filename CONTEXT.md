# Gateway

A framework for routing and proxying HTTP requests, with extensions for behavior outside the minimal gateway responsibilities. This glossary distinguishes request execution from observations and application-level conclusions.

## Language

**Core**:
The part of the gateway that serves, routes, and proxies HTTP and that owns HTTP correctness and hardening. It ships secure defaults that users may deliberately change. It contains no business policy.
_Avoid_: Minimal proxy (it is not minimal in safety)

**Business policy**:
A rule about who may call what, and how much, such as authentication, authorization, rate limits, quotas, or billing. Business policy never lives in the core; the project may offer it as optional packages.
_Avoid_: Hardening, security default

**Secure default**:
A protection the core enables without configuration because a safe value exists for nearly every workload. Users may deliberately relax it.

**Configurable protection**:
A protection the core provides but leaves off until configured, because a safe value depends on the deployment.
_Avoid_: Optional extension (it is core, not an extension)

**Managed header**:
A header whose meaning the gateway or an extension owns, such as a forwarding or identity header. The core removes managed headers from inbound requests before any extension sets them.
_Avoid_: Trusted header

**Extension point**:
A place where the core lets outside code take part in handling: middleware, lifecycle hooks, the transport, and upstream selection.

**Middleware**:
An extension point that wraps HTTP handling and may end it early.

**Lifecycle hook**:
An extension point that runs at a named phase of proxy execution with access to proxy-specific state.
_Avoid_: Plugin, event handler

**Processing hook**:
A lifecycle hook that takes part in handling before completion: when the request is received, before the upstream, or at the upstream response. A completion hook only observes.
_Avoid_: Filter

**Hook provider**:
A value that an extension registers with the gateway to supply its lifecycle hooks or to declare its managed headers. It is a part of an extension, not the extension itself.
_Avoid_: Extension (for the registered value), plugin

**Transport**:
An extension point that carries the outbound request to the selected upstream.

**Extension**:
Any package that plugs into one or more extension points using only the public contract. The extension points it uses describe it; there is no separate category.
_Avoid_: Plugin, module

**Bundled extension**:
An extension maintained with the project. It has no access to the core beyond the public contract.
_Avoid_: Built-in, first-party plugin

**Distribution**:
A runnable gateway assembled from the core and chosen extensions, with its own configuration and operational defaults. It is a consumer of the framework, not part of it.
_Avoid_: The gateway (ambiguous with the core)

**Route**:
A configured match on incoming requests that determines which upstream and which extensions apply. Groupings of routes, such as an API product, are deployment labels, not core concepts.
_Avoid_: API, API definition, endpoint

**Route binding**:
The attachment of an extension, with its route-specific settings, to one route. An extension without a binding on a route does not take part in that route's handling.
_Avoid_: Route middleware (a different mechanism), route attribute

**Configuration**:
The routes, extensions, and protections fixed when a gateway starts. Changing it requires a new gateway, never an in-place edit.
_Avoid_: Policy data

**Hardened server**:
The server that the core provides to serve a gateway with the server-level secure defaults: header limits, HTTP/2 limits, transfer idle deadlines, and shutdown. A gateway in another server does not get them.
_Avoid_: Edge server (edge is the deployment position, not this component)

**Transfer idle deadline**:
A limit on the time that one read or write of a body, or an upgraded connection, can make no progress. Each progress starts the time again. A handler that sets its own deadline for a direction takes over that direction for its request.
_Avoid_: Read timeout, write timeout (these limit the total time in `net/http`)

**Upgraded connection**:
A client connection that the gateway took from `net/http` to forward a protocol switch (101 Switching Protocols). The hardened server applies transfer idle deadlines to it and closes it at shutdown. A connection that other code takes with `Hijack` is not an upgraded connection of the gateway.
_Avoid_: Tunnel; hijacked connection, when the owner of the connection is important

**Upstream selector**:
An extension point that chooses the upstream URL for a matched request. The default chooses the route's single static upstream URL.
_Avoid_: Load balancer (one possible selector)

**Upstream URL**:
The absolute URL that an upstream selector returns for a request. Its path is a base path: the gateway adds the inbound path to it.
_Avoid_: Target (ambiguous with request target), backend

**Request target**:
The target in the request line (RFC 9112, section 3.2). The core accepts only the origin form: a path with an optional query.
_Avoid_: Target (alone)

**Path policy**:
The checks on the request path before routing. Reject mode refuses paths that an upstream can decode to a path different from the path that the router matched.

**Identity header**:
A header that states who the client is or where the request came from, such as `X-Forwarded-For` or `True-Client-IP`. A client can spoof it, so the core removes it from inbound requests and sets the forwarding headers from the direct peer.

**Rejection**:
A deliberate refusal to continue handling, carrying a status, response headers, and a public reason. It is distinct from an unexpected failure.
_Avoid_: Error (for deliberate refusals)

**Rejection reason**:
A public code that names the check that refused a request, never the value that failed it. It contains no request content.
_Avoid_: Error message, rejection detail

**Gateway-managed handling**:
The processing of an inbound request governed by the gateway, including its registered extensions. It excludes work performed independently by callers outside that boundary.

**Request completion**:
The end of gateway-managed handling, whether it finishes normally or aborts. It does not establish that the client received the entire response or that a usage event was durably recorded.
_Avoid_: Delivery acknowledgment, billing confirmation

**Selected upstream**:
The destination chosen for a request. Selection alone does not prove that a connection was attempted or that the destination received the request.
_Avoid_: Contacted upstream, unless contact is independently established

**Execution fact**:
Information established by gateway execution, such as a matched route, a selected upstream, an explicit rejection, or an observed failure. It does not include inferred delivery or application-level meaning.

**HTTP observation**:
An observed HTTP status, accepted response-body byte count, or response-write error at a particular observation point. Upstream and downstream observations may differ and do not guarantee complete client delivery.

**Interpretation**:
A classification derived from execution facts and HTTP observations for purposes such as logging, analytics, or billing. It is not itself an authoritative fact about gateway execution.
