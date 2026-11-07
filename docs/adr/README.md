# Architecture Decision Records (ADRs)

These ADRs separate framework choices from specific API-management features. The 2026-11-01 direction review accepted the remaining proposed ADRs or superseded them with ADRs 0011–0016. See the [roadmap](../roadmap.md) for the planned order of work.

Acceptance does not freeze precise Go interfaces or mean every preferred extension point is implemented. The current APIs remain provisional, and implementation gaps are recorded in the relevant follow-up sections. See [the context map](../../CONTEXT-MAP.md) for the core and access-policy glossaries.

## ADRs

| ADR | Status | Decision | Main point |
| --- | --- | --- | --- |
| [0001](0001-framework-scope.md) | Superseded by 0011 | Framework scope | Minimal, embeddable proxy core; business features live in extensions |
| [0002](0002-use-go-standard-http-proxy.md) | Accepted | HTTP foundation | `net/http` and `httputil.ReverseProxy` rather than a custom networking stack |
| [0003](0003-proxy-lifecycle-hooks.md) | Accepted (amended by 0019, superseded in part by 0021 and 0030) | Lifecycle hooks | Small optional interfaces compiled into ordered hook lists at startup |
| [0004](0004-compose-http-middleware-and-hooks.md) | Accepted (amended by 0018, superseded in part by 0021 and 0030) | Middleware coexistence | Route middleware enters before route-matched hooks; completion surrounds the managed chain |
| [0005](0005-per-request-gateway-context.md) | Accepted | Request context | Typed extension state, serial per-request access, and explicit context lifetimes |
| [0006](0006-compile-time-go-extensions.md) | Accepted | Extension distribution | Compile-time Go packages before dynamic plugin systems |
| [0007](0007-immutable-runtime-pipelines.md) | Accepted | Configuration lifecycle | Validate and compile immutable request pipelines at startup; policy data changes at runtime |
| [0008](0008-separate-routing-from-upstream-selection.md) | Accepted | Upstream choice | Route matching is distinct from destination selection; the upstream selector is an extension point |
| [0009](0009-outcomes-errors-and-early-responses.md) | Accepted (amended by 0013, 0018) | Outcome semantics | Core execution facts, middleware HTTP observations, extension interpretations; completion is not delivery |
| [0010](0010-scaffold-hardening-and-verification.md) | Superseded by 0012 | Verification | Test correctness/security boundaries, without claiming production readiness |
| [0011](0011-library-first-gateway-framework.md) | Accepted | Project direction | Library-first core with correctness and hardening; business policy in unprivileged bundled extensions |
| [0012](0012-secure-defaults-and-configurable-protections.md) | Accepted (refined by 0017, 0023, 0024, 0025, 0026) | Protections | Secure defaults on; deployment-dependent protections provided but off until configured |
| [0013](0013-typed-rejections-and-replaceable-renderer.md) | Accepted (amended by 0022) | Rejections | Rejections carry headers; replaceable renderer with a `problem+json` default |
| [0014](0014-multi-module-repository.md) | Accepted | Modules | Dependency-light core; bundled extensions with dependencies get their own modules |
| [0015](0015-access-policy-model.md) | Accepted | Access policy | Limits per subject, evaluated independently, consumed at admission |
| [0016](0016-core-owns-the-inbound-server.md) | Accepted (refined by 0024) | Serving edge | The core provides the hardened server; a bare handler loses server-level protections |
| [0017](0017-failure-status-by-source.md) | Accepted (superseded in part by 0022, refined by 0023) | Failure statuses | The source of a failure sets the status: 500 for extension code, 502 for the upstream round trip |
| [0018](0018-completion-observes-gateway-rejections.md) | Accepted (amended by 0022, 0023, 0025) | Completion boundary | Completion observes gateway-owned rejections with a public reason code and no request content |
| [0019](0019-hook-outputs-and-destination-ownership.md) | Accepted (amended by 0027) | Hook outputs | Hooks cannot change the destination; the core sanitizes header maps after hooks |
| [0020](0020-route-prefix-matching-without-redirects.md) | Accepted | Route matching | Escaped, case-sensitive, longest prefix match; no trailing-slash redirects |
| [0021](0021-hook-order-by-provider-position.md) | Superseded by 0030 | Hook order | The first hook provider is the outermost one; response phases run in reverse order |
| [0022](0022-rejections-and-the-problem-response.md) | Accepted | Rejections and renderer | Processing hooks return `(*Rejection, error)` with `<extension>.<check>` codes, and an error always wins; a body-only renderer with a minimal `problem+json` default |
| [0023](0023-failure-evidence-and-transport-facts.md) | Accepted (amended by 0025, 0027, refined by 0028) | Failure evidence | 499 and 504 from typed errors and the client context; transport trace facts; protocol-switch failures; no response on a connection that another component hijacked |
| [0024](0024-hardened-server-deadlines-and-upgraded-connections.md) | Accepted | Hardened server | `NoTimeout` is the only way to disable a timeout; a handler's deadline call takes over its direction; writes have a deadline for each 16 KiB; the server closes the gateway's upgraded connections at shutdown |
| [0025](0025-core-protections-body-limit-admission-and-egress.md) | Accepted (amended by 0027, refined by 0028) | Core protections | A 10 MiB body-limit default with route limits; admission budgets without a queue; an egress transport that dials only checked literals; 413 from the overflow evidence; `DialError` for the egress dialer |
| [0026](0026-relax-request-secure-defaults.md) | Accepted (amended 2026-11-06) | Relaxations | Gateway-wide `QueryUnchecked`, `HeaderNameDrop`, and `AllowAbsoluteForm`; the other request checks cannot be relaxed |
| [0027](0027-request-trailers-at-the-end-of-the-body.md) | Accepted (refined by 0028) | Request trailers | Own trailer map; check all trailers at a successful end of the body; publish to middleware and the outbound request; 400 from the late-name evidence |
| [0028](0028-response-body-failures-at-completion.md) | Accepted | Response-body failures | Original read errors after the response hooks reach completion in `PhaseResponseBody`, also after a commit; no change to rendering or abort behavior |
| [0029](0029-access-log-extension-contract.md) | Accepted (registration superseded in part by 0030) | Access-log extension | A `Sink` interface called synchronously at completion; a recovered sink panic drops the record; a `Record` with safe and sensitive fields and a `LogValue` of the safe fields; a record also without middleware; a generated `X-Request-ID` |
| [0030](0030-typed-hook-phase-lists.md) | Accepted | Hook registration | Independent typed phase lists in execution order; separate header-owner declarations |
| [0031](0031-route-first-request-bodies.md) | Accepted | Route-first request bodies | Fixed matching before shared processing; route-aware restricted hooks; bounded memory buffering and request-scoped replay |

[ADR-0031](0031-route-first-request-bodies.md) supersedes the handling-order and body-processing parts of ADR-0003, ADR-0004, ADR-0018, and ADR-0025, and the affected rewrite and observation contracts in ADR-0019, ADR-0027, ADR-0029, and ADR-0030. It fixes route selection before body processing and defines explicit bounded memory buffering.

## Decision process

- **Proposed:** A candidate direction that can still change based on experiments.
- **Accepted:** Architectural direction explicitly approved; Go APIs may remain provisional and some implementation work may be deferred.
- **Superseded:** Replaced by a later ADR; link to its replacement.
- **Rejected:** Considered but not adopted.

Do not silently change the rationale of accepted ADRs. Write a new ADR to supersede a significant decision.

## Remaining provisional APIs and deferred work

1. Go hook signatures, typed `Config.Hooks` phase lists, `Config.HeaderOwners`, and function adapters (ADR-0003, ADR-0030). Four hook interfaces and their function adapters exist. The API stays provisional until v1.
2. Typed rejections and typed failures (ADR-0022, ADR-0023, ADR-0025, and ADR-0028). The `(*Rejection, error)` hook results, `Renderer`, `Problem`, the failure sentinels including `ErrInvalidRejection`, `ErrRequestBodyTooLarge`, `ErrInvalidTrailerName`, and `ErrEgressDenied`, and the transport facts in `Completion`. R10 is covered in part: only the egress transport returns a `DialError`.
3. State-key API (ADR-0005): `NewStateKey`, `SetState`, and `State`. Asynchronous middleware requires revisiting the lifecycle contract.
4. Route-specific hook registration (ADR-0004, roadmap section 3) remains deferred. ADR-0031 supersedes the old ordering: restricted RouteMatched hooks run after protection checks and admission, before buffering and all processing middleware.
5. Route-aware checks are implemented as RouteMatchedHook (ADR-0031), replacing RequestReceivedHook. They cannot change the request or read its body. A match alone does not establish invocation.
6. The server API (ADR-0016, ADR-0024): `Server`, `ServerConfig`, `NewServer`, and `NoTimeout`. TLS termination and certificate management are not decided.
7. The protection API (ADR-0025 and ADR-0026): `BodyLimit`, `MaxBodyBytes`, `NoBodyLimit`, `DefaultBodyLimit`, `AdmissionBudget`, `EgressPolicy`, `EgressTransport`, `Resolver`, `DialError`, `QueryPolicy`, `HeaderNamePolicy`, and `AllowAbsoluteForm`. The default body limit can change in a later release, with a stated reason.
8. The bundled extensions (ADR-0029): `accesslog.Recorder`, `Config`, `Sink`, `SinkFunc`, `Record`, and `RequestIDHeader`; and the outbound-signing demonstration `outboundsign.Signer`, which is not an authentication protocol. Its route key comes from route middleware, which does not decide between route bindings and route middleware (item 4).

## Experiments of the first implementation

The shared-context and access-log experiments of the first implementation (tag `prototype-final`) demonstrated:

- One gateway context accessible from global middleware, route middleware, and hooks.
- Private typed state with isolation across concurrent requests.
- Upstream response observation and completion after normal handling, early exits, and aborts.
- Middleware-owned status/byte accounting and trace-correlation snapshots without expanding the core outcome API.
- Optional response-writer instrumentation tested for flushing, informational responses, streaming failures, and connection upgrades.

The access-log middleware changed an inbound request header that was forwarded upstream; it did not validate a hook on the actual rewritten outbound request.

The [outbound-signing experiment](https://github.com/go-edgekit/edgekit/blob/prototype-final/extensions/outboundsign/README.md) separately demonstrated:

- Route middleware supplying private typed state to a provisional `BeforeUpstreamHook` after selection and rewriting.
- The upstream receiving signatures for the rewritten destination, method, effective Host, and escaped URI, including base-path and query changes.
- Ordered fallible outbound hooks before transport execution, with body closure and no delegation on hook failure.
- Error redaction, controlled rejections, cancellation, concurrent route isolation, and completion after middleware unwinds, including outbound-hook panics.

Signing did not require a route-matched hook. When a route-matched hook is needed, test that route middleware establishes state before it runs. The experiments did not establish interface stabilization, production signing security, live-span annotation, asynchronous middleware, or durable billing.
