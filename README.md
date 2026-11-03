# edgekit

Building blocks for API gateways in Go. The core uses `net/http` and `httputil.ReverseProxy`, and has no third-party dependencies.

edgekit is not a CDN, an edge-computing platform, or a cache. It serves, routes, and proxies HTTP at the edge of your API.

## Status

A rewrite replaces the first prototype in slices. See the [roadmap](docs/roadmap.md) and the [rewrite ledger](docs/rewrite-ledger.md). Slice 1, the proxy core, is done: routing, upstream selection, forwarding, header handling, the transport, and the request secure defaults.

These parts are not in the core yet:

- Lifecycle hooks and request state (slice 2).
- Typed rejections and the `problem+json` renderer (slice 3). Until then, the gateway sends plain-text error responses that contain only the status text.
- The hardened server (slice 4). Until then, configure server-level limits on your own `http.Server`, such as `ReadHeaderTimeout` and `MaxHeaderBytes`.
- Body limit, admission, and egress as core protections (slice 5). The packages in [`extensions/`](extensions) are prototype code until then.

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

server := &http.Server{
    Addr:              ":8080",
    Handler:           gateway,
    ReadHeaderTimeout: 5 * time.Second,
    MaxHeaderBytes:    64 << 10,
}
```

The [`Example` functions](example_test.go) show a static route, a custom upstream selector, a selector function, and a changed transport.

Middleware follows the `net/http` rule: it must not change the request that it gets. To change a request, call `r.Clone` and give the copy to the next handler.

The extension points are:

- **Middleware:** `func(http.Handler) http.Handler`, global or for each route.
- **Upstream selector:** chooses the upstream URL for a matched request. `StaticUpstream` is the default. `UpstreamSelectorFunc` makes a selector from a function.
- **Transport:** an `http.RoundTripper`. `NewTransport` gives the transport with the secure defaults.

## Secure defaults

Before any middleware runs, the gateway rejects `CONNECT`, request targets that are not in origin form, paths with dot-segments or encoded `.`, `/`, `\`, NUL, or `#`, queries that `url.ParseQuery` rejects (`;`, malformed percent-escapes, or more than 10000 parameters), so middleware and the upstream see the same parameters, and header or trailer names with characters other than letters, digits, and `-`. It removes forwarding and identity headers from the client, and headers that the configuration declares as managed, also from the names of request trailers. It removes hop-by-hop fields and `Alt-Svc` from responses, informational responses, and trailers. The transport has bounded timeouts, uses no environment proxy, and does no transparent compression.

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
- [Investigations](docs/investigations): surveys and assessments that the decisions use.
