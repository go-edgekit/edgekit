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
   - R10 is covered in part: the original failure and the trace facts. A typed dial failure for the failed attempt comes with the egress dialer (slice 5).
4. Core-owned hardened server, with a testable `Example` and an `examples/` program. Slice 1 removed the prototype `cmd/gateway` program.
   - Upgraded connections: `ReverseProxy` hijacks the connection of a 101 response, and `http.Server.Shutdown` does not wait for or close hijacked connections. The server tracks them and closes them at shutdown, optionally after a delay, and the idle deadlines (R9) also apply to them. Caddy has `stream_timeout` and `stream_close_delay` for the same gap.
5. Body limit, admission, and egress as core protections. Configuration to relax each request secure default from slice 1.
6. Bundled extensions: access log and outbound signing. Slice 1 removed the `headers` and `requesttag` demonstrations.

The rewrite is done when every ledger row is covered or dropped, the ledger and its AGENTS.md rule are deleted, no document calls the code a prototype, and `just check` passes. The Go API stays provisional until v1.

Scope of the rewrite:

- Public module path and Apache-2.0 license ([ADR-0014](adr/0014-multi-module-repository.md)). Done: `github.com/go-edgekit/edgekit` with the Apache-2.0 license.
- Secure defaults R1–R5, R7, R8, and R9; opaque/absolute-form/`CONNECT` rejection; no transparent transport compression; extension-managed headers; body limit, admission, and egress as core protections ([ADR-0012](adr/0012-secure-defaults-and-configurable-protections.md)).
- Core-owned hardened server ([ADR-0016](adr/0016-core-owns-the-inbound-server.md)).
- Typed dial and request-written failures (R10) as execution facts. Done in part: the request-written and connection facts, and evidence-based 499 and 504 ([ADR-0023](adr/0023-failure-evidence-and-transport-facts.md)). The typed dial failure is pending for the egress dialer.
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
- Analytics: the access-log extension with a sink interface; exporters as separate modules.

## Deferred

Distribution with declarative configuration, configuration hot reload, bundled load balancing and health checks, body transformations, importer for other gateway configuration formats.

Error responses: negotiation by `Accept` with `Vary`, and public data on a rejection for the renderer. Add each when an extension needs it; see the [error response formats investigation](investigations/error-response-formats.md).

Own proxy loop instead of `ReverseProxy` ([ADR-0002](adr/0002-use-go-standard-http-proxy.md)): the core corrects `ReverseProxy` in a few places (1xx headers, `Content-Length`, trailers, the 101 protocol switch, the source of protocol-switch failures, the close of the upstream connection after some of them, a nil body from a response hook, and the `Upgrade` check, which repeats a predicate of `ReverseProxy` before the proxy runs). Review ADR-0002 when a correction must do a step of `ReverseProxy` again instead of a step around it. When Go fixes golang/go#81207 and golang/go#81319, remove the matching parts of `proxyWriter`.
