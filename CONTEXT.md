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

**Extension point**:
A place where the core lets outside code take part in handling: middleware, lifecycle hooks, the transport, and later upstream selection.

**Middleware**:
An extension point that wraps HTTP handling and may end it early.

**Lifecycle hook**:
An extension point that runs at a named phase of proxy execution with access to proxy-specific state.
_Avoid_: Plugin, event handler

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

**Upstream selector**:
An extension point that chooses the destination for a matched request. The default chooses the route's single static target.
_Avoid_: Load balancer (one possible selector)

**Rejection**:
A deliberate refusal to continue handling, carrying a status, response headers, and a public reason. It is distinct from an unexpected failure.
_Avoid_: Error (for deliberate refusals)

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
