# Direct upstream destination policy

`egress` is an optional transport package for explicit origin and IP-network allowlists. It uses the existing `gateway.Config.Transport` boundary; it adds no framework configuration or lifecycle hooks. It is opt-in and is **not enabled by the demo executable**. A deployment must choose its actual allowed destinations, including private networks where needed.

## Registration

```go
// Configure operational settings once before constructing the owned transport.
template := http.DefaultTransport.(*http.Transport).Clone()
template.ResponseHeaderTimeout = 10 * time.Second
template.MaxIdleConns = 100
template.MaxIdleConnsPerHost = 16
template.MaxConnsPerHost = 64

transport, err := egress.New(template, egress.Config{
    AllowedOrigins: []string{"https://orders.internal.example:8443"},
    AllowedNetworks: []netip.Prefix{
        netip.MustParsePrefix("10.20.4.0/24"),
    },
    Dialer: &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second},
})
if err != nil {
    return err
}
defer transport.CloseIdleConnections()

app, err := gateway.NewWithConfig(gateway.Config{
    Routes:    routes,
    Transport: transport,
})
```

Here `egress` is `example.com/go-gateway-starter/extensions/egress`; `netip` is `net/netip`. The snippet belongs inside application setup. `routes` must use the allowed origins, with any required base paths and queries. Origins themselves cannot include base paths or queries.

Both allowlists are required. IPv4/IPv6 CIDRs are explicit permissions, not exclusions: broad `/0` prefixes grant broad access. Loopback, private, link-local, and metadata-service addresses have no implicit permission or blanket prohibition; permit only the networks the deployment needs. Use IPv4 prefixes for IPv4-mapped addresses. Unspecified, multicast, and scoped IP addresses are always rejected.

## Established boundaries

- **Origin validation:** only HTTP(S) scheme/host/effective-port combinations in `AllowedOrigins` can execute. Each request is checked before pool delegation, including when a connection already exists. Credentials, opaque URLs, malformed authorities, and fragments are rejected. Origins accept an optional trailing slash, not paths, queries, or fragments.
- **Canonical comparison:** DNS names are ASCII and case-insensitive; IDNs must use A-labels. DNS trailing dots, wildcard names, and underscores are rejected. Default ports, numeric port spellings, IPv6 spellings, and mapped IPv4 addresses are normalized for comparison. The actual request URL and Host are not rewritten.
- **Connection-time validation:** each new hostname connection resolves again. Every returned address must be allowed before any address is dialed. An empty, invalid, or mixed allowed/denied answer fails closed. IP-literal destinations are validated without DNS.
- **No second hostname lookup:** only validated IP literals reach the dialer, including connection fallback attempts. HTTP Host, TLS SNI, and certificate verification still use the request hostname. A TLS `ServerName` override in the template is rejected. Other TLS settings are retained as supplied; do not disable certificate verification.
- **No alternate transport path:** the package builds a clean owned `http.Transport`. It copies the template's TLS configuration, TLS/header/idle/expect-continue timeouts, connection-pool limits, compression/keepalive flags, read/write buffer sizes, response-header size limit, and `ForceAttemptHTTP2`. It does not copy proxy callbacks, proxy headers, dial callbacks, registered alternate protocols, or `TLSNextProto` callbacks. Ambient proxy variables are not used. Existing pools and custom HTTP protocol settings are not inherited.
- **Bounded connection work:** the supplied `net.Dialer` is copied and must have a positive `Timeout`. One context budget covers resolution and all sequential connection attempts, also respecting its absolute deadline and the incoming context. Nil uses a 30s timeout and 30s TCP keepalive. The connection budget does not limit response streaming. Standard transport cancellation/pool semantics still apply; a dial can outlive the request that initially triggered it when the standard transport retains it for reuse.
- **Ownership:** reuse the returned transport and close idle connections when appropriate. Configure TLS root pools, resolvers, callbacks, and other referenced objects before use and leave them immutable. Top-level settings and allowlist slices are copied, but custom Go callbacks and all referenced objects are not deeply isolated.

## Errors and lifecycle

Policy denials return `egress.ErrDenied`, without including the rejected URL or address. `errors.Is` works through the gateway's existing error handling. A denied origin closes the request body without reading it or delegating. Lookup and connection errors retain their original causes; they may contain addresses and are not automatically safe to log.

The gateway currently renders a generic 502 for a denial before response commitment. Completion runs after registered middleware unwinds, and `Outcome.Err` retains the error. Selected-upstream metadata remains a selection fact, not evidence of a connection attempt. The package does not add failure classification to the core or change signing's provisional error semantics.

Tests cover configuration validation, origin matching, IPv4/IPv6 and mapped addresses, mixed DNS answers, changed DNS answers on later connections, cancellation and connection budgets, literal-only fallback, request-body closure, configuration copying, concurrent use, standard HTTP/2, TLS verification/SNI, connection reuse, and bypass attempts through template proxy/dial/protocol callbacks. Integration tests run through the gateway and retain completion/error behavior.

## Limits and deployment responsibilities

This narrows direct outbound destinations; it is not a complete SSRF defense or a sandbox for compiled-in extensions.

- Origin checks concern the outbound URL, not an independent request `Host` override, application-level forwarding instructions, or upstream behavior. Only allow trusted origins and use separate transports where routes need different network permissions. The initial network list applies to all allowed origins, not separately to each origin.
- DNS answers are snapshots for a connection. Existing allowed connections can be reused after DNS changes. Replacing a policy requires a new transport and deliberate retirement of old connections; `CloseIdleConnections` does not terminate active streams or upgraded connections. There is no reload implementation.
- Narrow IP permissions still require network-level egress controls. NAT, transparent proxies, service meshes, routing changes, trusted dialer callbacks, and upstream servers can affect where traffic ultimately goes. DNS queries themselves are not restricted by the origin IP-network allowlist. A custom resolver/dialer/TLS callback is trusted code, not isolated policy input.
- Connections use sequential fallback, not a custom Happy Eyeballs implementation. A slow first address can consume the connection budget. DNS policy and availability tradeoffs need workload-specific testing.
- The transport does not follow redirects; the gateway forwards upstream redirect responses. Clients may independently follow them. Redirect-response filtering would be a separate application policy.
- Network policy does not authenticate clients, validate upload semantics, redact all logs, bound aggregate concurrency, or establish production readiness. See [the follow-up plan](../../docs/investigations/proxy-risk-follow-ups.md).
