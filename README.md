# edgekit

Building blocks for API gateways in Go. The core uses `net/http` and `httputil.ReverseProxy`, and has no third-party dependencies.

edgekit is not a CDN, an edge-computing platform, or a cache. It serves, routes, and proxies HTTP at the edge of your API.

## Status

A rewrite replaces the first prototype in slices. See the [roadmap](docs/roadmap.md) and the [rewrite ledger](docs/rewrite-ledger.md). Slice 1, the proxy core, is done: routing, upstream selection, forwarding, header handling, the transport, and the request secure defaults. Slice 2 is done: lifecycle hooks, typed request state, completion with execution facts, and managed headers that extensions declare. Slice 3 is done: typed rejections from hooks, a replaceable renderer with a minimal `problem+json` default, and failure statuses from evidence, including 499 and 504. Slice 4 is done: the hardened `Server`, with header limits, HTTP/2 limits, transfer idle deadlines, and graceful shutdown that also closes upgraded connections. Slice 5 is done: the request-body limit as a secure default, admission budgets and the egress transport as configurable protections, and explicit relaxations of the request secure defaults. Slice 6 is done: the access-log extension and the outbound-signing demonstration, as bundled extensions that use only the public API.

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
    Routes: []edgekit.Route{{
        Prefix:   "/api/",
        Upstream: upstream,
    }},
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

- **Middleware:** `func(http.Handler) http.Handler`, global or for each route.
- **Upstream selector:** chooses the upstream URL for a matched request. `StaticUpstream` is the default. `UpstreamSelectorFunc` makes a selector from a function.
- **Transport:** an `http.RoundTripper`. `NewTransport` gives the transport with the secure defaults. `NewEgressTransport` gives a transport that connects only to allowed origins and IP networks.

## Bundled extensions

Bundled extensions use only the public API, as your own extensions do:

- [`extensions/accesslog`](extensions/accesslog): one record for each request, with the HTTP observations of its middleware, the trace ID, and the execution facts of completion. A `Sink` that you supply gets the records. A `Record` logged with `log/slog` writes only the fields that are safe by default ([ADR-0029](docs/adr/0029-access-log-extension-contract.md)).
- [`extensions/outboundsign`](extensions/outboundsign): a demonstration that signs the outbound request of a route with HMAC-SHA256. It is not an authentication protocol: it has no timestamp, no replay protection, and no body integrity.

## Secure defaults

Before any middleware runs, the gateway rejects `CONNECT`, request targets that are not in origin form or that have a fragment (an unescaped `#`), paths with dot-segments or encoded `.`, `/`, `\`, NUL, or `#`, queries that `url.ParseQuery` rejects (`;`, malformed percent-escapes, or more than 10000 parameters), so middleware and the upstream see the same parameters, and header or trailer names with characters other than letters, digits, and `-`. It removes forwarding and identity headers from the client, and headers that the configuration declares as managed, also from the names of request trailers. It removes hop-by-hop fields and `Alt-Svc` from responses, informational responses, and trailers. The transport has bounded timeouts, uses no environment proxy, and does no transparent compression.

The gateway limits request bodies to 10 MiB. This is an initial default for ordinary API requests, not a value that is safe for all workloads, and a later release can change it with a stated reason. Set `Route.BodyLimit` for upload routes, and set the limit explicitly when you need an exact value. `edgekit.NoBodyLimit()` disables the limit explicitly. The gateway does not buffer the body. It sends 413 for a declared length over the limit, or for a streaming overflow that causes a failure before the final response headers are sent. The upstream can receive part of the body before the limit stops it. If an overflow reaches the response-body reader after the final response headers were sent, the response aborts instead. An upstream that responds before the overflow can finish the exchange without a failure.

The `Server` limits the header-read time (10s) and the header size (64 KiB), limits HTTP/2 to 100 streams for each connection with a 64 KiB stream window and a 1 MiB connection window, and stops a body transfer or an upgraded connection that makes no progress for 300s. It uses no total read or write timeouts, so long streams that make progress continue. Each value can change, and `edgekit.NoTimeout` disables a timeout explicitly; an omitted value always keeps the default. The `Server` GoDoc in [server.go](server.go) gives the rules.

Three relaxations exist for legacy clients, each off by default and for the complete gateway: `QueryUnchecked` accepts any query, `HeaderNameDrop` removes invalid header names instead of a rejection, and `AllowAbsoluteForm` accepts absolute-form request targets.

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
