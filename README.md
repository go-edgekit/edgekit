# edgekit

Building blocks for API gateways in Go. The core uses `net/http` and `httputil.ReverseProxy`, and has no third-party dependencies.

edgekit is not a CDN, an edge-computing platform, or a cache. It serves, routes, and proxies HTTP at the edge of your API.

## Status

A rewrite replaces the first prototype in slices. See the [roadmap](docs/roadmap.md) and the [rewrite ledger](docs/rewrite-ledger.md). Slice 1, the proxy core, is done: routing, upstream selection, forwarding, header handling, the transport, and the request secure defaults. Slice 2 is done: lifecycle hooks, typed request state, completion with execution facts, and managed headers that extensions declare. Slice 3 is done: typed rejections from hooks, a replaceable renderer with a minimal `problem+json` default, and failure statuses from evidence, including 499 and 504. Slice 4 is done: the hardened `Server`, with header limits, HTTP/2 limits, transfer idle deadlines, and graceful shutdown that also closes upgraded connections. Slice 5 is done: the request-body limit as a secure default, admission budgets and the egress transport as configurable protections, and explicit relaxations of the request secure defaults. Slice 6 is done: the access-log extension and the outbound-signing demonstration, as bundled extensions that use only the public API. Route-first request-body processing and explicit bounded memory buffering are implemented under [ADR-0031](docs/adr/0031-route-first-request-bodies.md).

These parts are not in the core yet:

- A typed dial failure for the transport of `NewTransport` (R10). The egress transport returns `DialError`; completion has the request-written and connection facts for all transports.
- Path and query rewriting, and normalize mode and the configuration of the path policy (slice 1b).
- Trusted proxies (R6) and idle-connection retirement (R11).

The Go API is provisional until version 1. The project is not ready for production use.

## Use it

```go
target, err := url.Parse("http://127.0.0.1:9090")
if err != nil {
    return err
}

upstream, err := edgekit.StaticUpstream(target)
if err != nil {
    return err
}

gateway, err := edgekit.New(edgekit.Config{
    Routes: []edgekit.Route{
        {
            Prefix:   "/api/",
            Upstream: upstream,
        },
    },
})
if err != nil {
    return err
}
defer gateway.CloseIdleConnections()

// The zero ServerConfig applies all secure defaults.
server, err := edgekit.NewServer(gateway, edgekit.ServerConfig{})
if err != nil {
    return err
}

listener, err := net.Listen("tcp", ":8080")
if err != nil {
    return err
}

go server.Serve(listener) // Returns http.ErrServerClosed after Shutdown.

// At the end: wait for the active requests, then stop the rest.
if err := server.Shutdown(ctx); err != nil {
    server.Close()
}
```

`Server` is the supported way to serve a gateway. A gateway in your own `http.Server` works, but then that server owns the header limits, the timeouts, and the upgraded connections.

The [`examples/gateway`](examples/gateway) program serves a gateway with signals, graceful shutdown, failure logs without error details, an access log of approved fields, and outbound signing. It requires `EDGEKIT_SIGNING_KEY`, with at least 32 bytes. Make a random key with `openssl rand -hex 32`: it gives 64 hexadecimal characters, which the program uses as the key bytes.

The [`Example` functions](example_test.go) show a static route, a custom upstream selector, a selector function, a changed transport, the server, the body limit, an admission budget, and the egress transport.

Middleware follows the `net/http` rule: it must not change the request that it gets. To change a request, call `r.Clone` and give the copy to the next handler.

The extension points are:

- **Middleware:** `func(http.Handler) http.Handler`, in `Config.SharedMiddleware` or `Route.Middleware`, after fixed route selection and body protection.
- **Lifecycle hooks:** `Config.Hooks` contains independent typed lists for `RouteMatched`, `BeforeUpstream`, `UpstreamResponse`, and `RequestCompleted`. Each list runs in its written order. See the testable [`ExampleHooks`](hooks_test.go).
- **Upstream selector:** chooses the upstream URL for a matched request. `StaticUpstream` is the default. `UpstreamSelectorFunc` makes a selector from a function.
- **Transport:** an `http.RoundTripper`. `NewTransport` gives the transport with the secure defaults. `NewEgressTransport` gives a transport that connects only to allowed origins and IP networks.

A hook runs only in the phase lists that contain it. Register managed-header declarations separately in `Config.HeaderOwners []HeaderOwner`, or list the names directly in `Config.ManagedHeaders`. Hook registration alone does not declare headers ([ADR-0030](docs/adr/0030-typed-hook-phase-lists.md)). Declare middleware-owned request headers too: without a declaration, client values can reach middleware, and client `Connection` options can remove headers that middleware sets before forwarding.

## Bundled extensions

Bundled extensions use only the public API, as your own extensions do:

- [`extensions/accesslog`](extensions/accesslog): one record for each request, with the HTTP observations of its middleware, the trace ID, and the execution facts of completion. A `Sink` that you supply gets the records. A `Record` logged with `log/slog` writes only the fields that are safe by default ([ADR-0029](docs/adr/0029-access-log-extension-contract.md)).
- [`extensions/outboundsign`](extensions/outboundsign): a demonstration that signs the outbound request of a route with HMAC-SHA256. It is not an authentication protocol: it has no timestamp, no replay protection, and no body integrity.

## Secure defaults

Before any middleware runs, the gateway rejects `CONNECT`, request targets that are not in origin form or that have a fragment (an unescaped `#`), paths with dot-segments or encoded `.`, `/`, `\`, NUL, or `#`, queries that `url.ParseQuery` rejects (`;`, malformed percent-escapes, or more than 10000 parameters), so middleware and the upstream see the same parameters, and header or trailer names with characters other than letters, digits, and `-`. It removes forwarding and identity headers from the client, and headers that the configuration declares as managed, also from the names of request trailers. It removes hop-by-hop fields and `Alt-Svc` from responses, informational responses, and trailers. The transport has bounded timeouts, uses no environment proxy, and does no transparent compression.

The gateway limits request bodies to 10 MiB. This is an initial default for ordinary API requests, not a value that is safe for all workloads, and a later release can change it with a stated reason. Set `Route.BodyLimit` for upload routes, and set the limit explicitly when you need an exact value. `edgekit.NoBodyLimit()` disables the limit explicitly. The gateway selects the effective route limit once, before any permitted body reader. The gateway default is inherited, not a ceiling. Streaming remains the default. The limiter counts exposed inbound body bytes, not transfer framing, server cleanup reads, decompressed output, or independently generated bodies. It sends 413 for a declared length over the limit, or for a streaming overflow that causes a failure before the final response headers are sent. The upstream can receive part of the body before the limit stops it. If an overflow reaches the response-body reader after the final response headers were sent, the response aborts instead. An upstream that responds before the overflow can finish the exchange without a failure.

The `Server` limits the header-read time (10s) and the header size (64 KiB), limits HTTP/2 to 100 streams for each connection with a 64 KiB stream window and a 1 MiB connection window, and stops a body transfer or an upgraded connection that makes no progress for 300s. It uses no total read or write timeouts, so long streams that make progress continue. Each value can change, and `edgekit.NoTimeout` disables a timeout explicitly; an omitted value always keeps the default. The `Server` GoDoc in [server.go](server.go) gives the rules.

Three relaxations exist for legacy clients, each off by default and for the complete gateway: `QueryUnchecked` accepts any query, `HeaderNameDrop` removes invalid header names instead of a rejection, and `AllowAbsoluteForm` accepts absolute-form request targets.

## Route-first request processing

The gateway validates and sanitizes the inbound request, acquires gateway admission, matches one route, installs its effective body limit, checks the declared length, acquires route admission, and runs restricted `RouteMatchedHook` checks. It then buffers only if configured, runs shared middleware and route middleware, selects the upstream, and forwards the request. Cleanup and admission release precede completion ([ADR-0031](docs/adr/0031-route-first-request-bodies.md)).

`RouteMatchedHook` cannot change the request or read its body. It can reject a header-only request before buffering. It runs only after matching, body-length checks, and admission; a match alone does not guarantee invocation. Later middleware context and state are not available yet.

The original route match remains fixed. A middleware change to a copied request's path/query is an outbound rewrite, never rematching. Invalid rewritten output fails internally with 500. Middleware that replaces or expands a body owns its generated output bounds and its closure, length, replay, and trailer fields.

Shared middleware does not run for route misses or earlier rejections. Completion observes them. The access log then reports `Observed: false` with execution facts, not an inferred status or a generated request ID.

### Explicit request buffering

Set `Config.RequestBuffering` to a `RequestBuffering` with a shared `RequestBufferBudget` from `NewRequestBufferBudget(bytes)` and positive, explicit `IdleTimeout` and `TotalTimeout` values. A nil route setting inherits that complete configuration. Set `Route.RequestBuffering` to replace it without field merging, or to `NoRequestBuffering()` to select streaming explicitly ([ADR-0032](docs/adr/0032-inherited-request-buffering.md)). A nil gateway setting, or `NoRequestBuffering()` at that level, keeps streaming as the default. Do not add a budget or timeouts to `NoRequestBuffering()`; `New` rejects conflicting settings. Each buffered route requires a finite effective body limit that fits its selected budget. All inheriting routes share the configured budget; no budget is created for each route. `New` copies the effective settings, so later configuration changes have no effect. A request with a body also requires the scoped deadline support of `Server`; missing support fails with 500 before reading, and `Completion.Failure` holds the cause. Streaming gateways and requests without a body do not have that requirement. See `ExampleRequestBuffering` for a complete setup and `ExampleNoRequestBuffering` for a streaming override. Shared middleware must not assume every route supplies `GetBody`. Temporary deadlines preserve stricter applicable bounds and restore their original ownership.

A request without a body skips buffering and reserves nothing. A request with a declared length reserves that length. A request with an unknown length reserves the complete effective route limit before reading: a 100 MiB route reserves 100 MiB even for a small chunked body. Reservation is not actual allocation. Capacity exhaustion rejects immediately with 503 and `request_buffer_saturated`. Memory-only storage grows one bounded chunk at a time, with no disk spill or response buffering. Reservations bound stored-body capacity, not process memory or runtime overhead. Ordinary rejection middleware runs after buffering; use restricted route-matched checks when it must run earlier.

The gateway clears caller-provided inbound `GetBody` on its request copy. Streaming supplies no gateway replay. After a checked EOF, buffered processing middleware gets `Body` and request-scoped `GetBody` cursors over shared storage. Repeated and concurrent replay is supported without complete-body copies or inbound recounting. Reading consumes a cursor; rewinding is explicit. The outbound transport always gets `GetBody == nil`, so buffering does not add replay retries. When the outbound request still has an unread gateway cursor, no trailer fields, and content of unknown length that is not empty, the upstream gets a declared `Content-Length` instead of chunked framing. An empty body of unknown length is not changed to length zero, because net/http can retry a request without a body.

Reservations last through protected handling, including a stalled or streaming response or an upgrade. Cleanup invalidates all gateway-owned replay handles, drops storage, then releases capacity. Do not retain bodies or replay functions after handling. Buffered failures never forward the body through the normal proxy path. Established truncation or invalid trailers give 400; established inbound read deadlines give 408; unknown reader errors give 500. Some Go chunk-decoding and HTTP/2 length-check errors lack typed evidence and therefore remain 500. Responses contain no failure details. See [RequestBuffering GoDoc](request_buffer.go) for the full contract and [the tests](request_buffer_test.go) for lifecycle examples. Indefinite request streams must use streaming routes.

## Configurable protections

These protections are off until you configure them, because a safe value depends on the deployment:

- **Admission:** an `AdmissionBudget` from `NewAdmissionBudget(n)` in `Config.Admission` or `Route.Admission` limits the requests that are active at the same time. A full budget gives 503 at once; there is no queue.
- **Egress:** an `EgressTransport` from `NewEgressTransport` in `Config.Transport` sends requests only to the allowed origins, and connects only to checked addresses in the allowed IP networks. It is not a complete defense against server-side request forgery; keep network controls outside the process.

The `Gateway` GoDoc in [gateway.go](gateway.go) gives the complete list, the order of request handling, and the deliberate deviations from the HTTP specifications. The [threat model](docs/threat-model.md) shows which threats these defaults address.

## Develop

Run all checks that CI runs. The Nix flake pins the tools:

```text
nix develop -c just check
```

Without Nix, install Go 1.26 or later, `just`, `golangci-lint`, `actionlint`, and `zizmor`, then run `just check`. For the tests only, run `go test ./...`.

## Documentation

- [ADRs](docs/adr/README.md): the target design.
- [Context map](CONTEXT-MAP.md) and [glossary](CONTEXT.md): the terms that the code and documents use.
- [Threat model](docs/threat-model.md): assets, trust boundaries, and mitigations.
- [Standards](docs/standards.md): the RFCs that the core uses, and the deliberate deviations.
- [Investigations](docs/investigations): surveys and assessments that the decisions use.
