# Roadmap

**Status:** Plan from the 2026-11-01 direction review. Easy to change; decisions live in the [ADRs](adr/README.md).

The current code is a prototype. It is replaced by a rewrite, not evolved. The existing tests are the behavior specification for the rewrite: lifecycle, proxy safety, access-log, egress, admission, and body-limit tests carry over in intent.

## 1. Settle direction

Done: ADRs 0011–0016, status changes for 0002 and 0006–0008, and the [context map](../CONTEXT-MAP.md).

## 2. Rewrite the core

The rewrite replaces the prototype in place on `main`, in slices. The prototype is kept at the git tag `prototype-final`. Each slice replaces code together with its tests, and `just check` passes after each slice. A slice removes extensions that the new core breaks, and their tests stay pending in the [rewrite ledger](rewrite-ledger.md) until a later slice rewrites them. Tests are rewritten against the new API, not ported, and the ledger tracks each prototype test.

Slices:

0. Groundwork: tag, ledger, and the rewrite rule in AGENTS.md. Done.
1. Proxy core: routing, the upstream-selector extension point with static selection, forwarding, managed and hop-by-hop headers, transport, and cancellation. Also the request secure defaults: rejection of opaque-form, absolute-form, and asterisk-form targets and `CONNECT`, the path policy in reject mode (R1), query-semicolon rejection (R2), header-name rejection (R3), and `Alt-Svc` removal (R7). Done.
   - 1b. Path and query rewriting on routes, and the path policy in normalize mode.
2. Lifecycle hooks and request state, including the rename of the hook-provider registration (`Config.Hooks`). Also completion of gateway-owned rejections, failure statuses by source, and the header rules after hooks ([ADR-0017](adr/0017-failure-status-by-source.md) to [ADR-0020](adr/0020-route-prefix-matching-without-redirects.md)). Done.
3. Typed rejections, the replaceable renderer, and typed failures (R10) ([ADR-0022](adr/0022-rejections-and-the-problem-response.md), [ADR-0023](adr/0023-failure-evidence-and-transport-facts.md)). Done.
   - The failures that `ReverseProxy` reported as `PhaseUnknown` have a source or a stage: a client `Upgrade` value that is not printable is a secure-default rejection with 400, a 101 body that cannot be written and a writer that cannot hijack are embedder defects with 500, and an upstream that does not accept the protocol switch is a failure in `PhaseProtocolSwitch` with 502.
   - R10 is covered in part: the original failure and the trace facts. Slice 5 adds the typed dial failure of the egress transport.
4. Core-owned hardened server, with a testable `Example` and an `examples/` program ([ADR-0024](adr/0024-hardened-server-deadlines-and-upgraded-connections.md)). Done.
   - Header-read timeout, header budget, HTTP/2 stream and window limits (R8), and transfer idle deadlines (R9) for requests and upgraded connections. Graceful shutdown closes the upgraded connections of the gateway before it waits for the other requests.
   - TLS from a caller's `tls.Config`. Certificate management and the TLS-termination choice stay open.
5. Body limit, admission, and egress as core protections, and explicit relaxations of the request secure defaults ([ADR-0025](adr/0025-core-protections-body-limit-admission-and-egress.md), [ADR-0026](adr/0026-relax-request-secure-defaults.md)). Done.
   - The body limit is a secure default of 10 MiB with a limit for each route. Admission budgets and the egress transport are configurable protections. `DialError` gives the attempts of a failed dial of the egress transport; `NewTransport` does not return it yet.
   - Request trailers are checked at the end of the body and forwarded to middleware and the upstream ([ADR-0027](adr/0027-request-trailers-at-the-end-of-the-body.md)).
   - Relaxations: `QueryUnchecked`, `HeaderNameDrop`, and `AllowAbsoluteForm`. `CONNECT`, the other target forms, the `Upgrade` check, and the outbound header check cannot be relaxed. The configuration of the path policy comes with normalize mode in slice 1b.
6. Bundled extensions: access log and outbound signing ([ADR-0029](adr/0029-access-log-extension-contract.md)). Slice 1 removed the `headers` and `requesttag` demonstrations. Done.
   - Both extensions use only the standard library, so they stay in the core module (ADR-0014). A lint rule forbids internal imports in `extensions/` and `examples/`.
   - The access log covers the sink part of section 4: a `Sink` interface that is called synchronously at completion. Exporters, bounded asynchronous sinks, sampling, and metrics stay in section 4.
   - Outbound signing is a demonstration, not a supported security extension: it has no timestamp, replay protection, body integrity, or key identifier. Its key comes from route middleware through request state, as in the prototype. This does not decide the open experiment of section 3.
   - A signing failure is 500 (a hook failure, ADR-0017), or 499 when the client is gone (ADR-0023), not the 502 of the prototype.
   - The rewrite is not finished: slice 1b and the typed dial failure of `NewTransport` are open, so the ledger and its rule stay.

The rewrite is done when every slice of this roadmap is done, every ledger row is covered or dropped, the ledger and its AGENTS.md rule are deleted, no document calls the code a prototype, and `just check` passes. The Go API stays provisional until v1.

Scope of the rewrite:

- Public module path and Apache-2.0 license ([ADR-0014](adr/0014-multi-module-repository.md)). Done: `github.com/go-edgekit/edgekit` with the Apache-2.0 license.
- Secure defaults R1–R5, R7, R8, and R9 (R8 and R9 done); opaque/absolute-form/`CONNECT` rejection; no transparent transport compression; extension-managed headers; body limit, admission, and egress as core protections ([ADR-0012](adr/0012-secure-defaults-and-configurable-protections.md)). Done: body limit, admission, and egress ([ADR-0025](adr/0025-core-protections-body-limit-admission-and-egress.md)).
- Core-owned hardened server ([ADR-0016](adr/0016-core-owns-the-inbound-server.md)). Done.
- Typed dial and request-written failures (R10) as execution facts. Done in part: the request-written and connection facts, evidence-based 499 and 504 ([ADR-0023](adr/0023-failure-evidence-and-transport-facts.md)), and the `DialError` of the egress transport ([ADR-0025](adr/0025-core-protections-body-limit-admission-and-egress.md)). A typed dial failure for `NewTransport` needs its own decision.
- Upstream-selector extension point with static selection ([ADR-0008](adr/0008-separate-routing-from-upstream-selection.md)).
- Typed rejections and replaceable `problem+json` renderer ([ADR-0013](adr/0013-typed-rejections-and-replaceable-renderer.md), [ADR-0022](adr/0022-rejections-and-the-problem-response.md)). Done.
- Path and query rewriting on routes; routes no longer always forward the original path.
- Rename the hook-provider registration so "extension" keeps its glossary meaning. Done: `Config.Hooks`.

## 3. Bundled access-policy extensions

Proves the motivating limitations can be solved without core changes ([ADR-0015](adr/0015-access-policy-model.md)):

1. Key authentication with hashed key store and in-memory adapter.
2. Rate limits (GCRA, sliding-window counter) and quotas (calendar periods) with a static limit source and in-memory store.
3. Redis store adapter as a separate module.
4. Header transformations. Body transformations need their own ADR.

**Open experiment:** how route-specific settings reach an extension. The working hypothesis is route bindings (register once, bind per route, per-route hook lists compiled at startup). Route middleware with shared request state is the fallback. Decide from the rate-limit extension, then record an ADR.

## 4. Remaining hardening and analytics

- R6 trusted-proxy mode, R11 idle-connection retirement.
- Analytics: the access-log extension with a sink interface is done (slice 6). Exporters as separate modules, bounded asynchronous sinks, and metrics stay open. Accepting a request ID from a trusted proxy needs R6.

## Deferred

Distribution with declarative configuration, configuration hot reload, bundled load balancing and health checks, body transformations, importer for other gateway configuration formats.

Error responses: negotiation by `Accept` with `Vary`, and public data on a rejection for the renderer. Add each when an extension needs it; see the [error response formats investigation](investigations/error-response-formats.md).

Own proxy loop instead of `ReverseProxy` ([ADR-0002](adr/0002-use-go-standard-http-proxy.md)): the core corrects `ReverseProxy` in a few places (1xx headers, `Content-Length`, trailers, the 101 protocol switch, the source of protocol-switch failures, the close of the upstream connection after some of them, a nil body from a response hook, and the `Upgrade` check, which repeats a predicate of `ReverseProxy` before the proxy runs). Review ADR-0002 when a correction must do a step of `ReverseProxy` again instead of a step around it. When Go fixes golang/go#81207 and golang/go#81319, remove the matching parts of `proxyWriter`.
