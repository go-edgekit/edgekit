# Proxy implementation survey: request paths, defaults, and hardening

**Status:** Investigation report, not an architecture decision or production-readiness claim

> **Update 2026-11-01:** The ownership column below predates the direction review. [ADR-0012](../adr/0012-secure-defaults-and-configurable-protections.md) and [ADR-0016](../adr/0016-core-owns-the-inbound-server.md) now classify these items: R1–R5 and R7–R9 are core secure defaults, R10 is part of the core rewrite, and R6 is a configurable protection. Open questions 1 and 2 are answered there. [ADR-0001](../adr/0001-framework-scope.md) is superseded by [ADR-0011](../adr/0011-library-first-gateway-framework.md).

This report compares how Go reverse proxies and gateways implement the proxy request path at the source level. It focuses on forwarding mechanics, defaults, and hardening. The [gateway survey assessment](high-over-findings.md) covers product scope and extension models; this report does not repeat it.

No ADR status, public API, or framework default changes because of this report. The [recommendations](#recommendations-for-this-project) are candidates for future focused passes, in the style of [the risk follow-ups](proxy-risk-follow-ups.md).

## Contents

1. [Scope and method](#scope-and-method)
2. [Summary](#summary)
3. [The canonical proxy request path](#the-canonical-proxy-request-path)
4. [Project notes](#project-notes)
5. [Default values compared](#default-values-compared)
6. [Hardening themes](#hardening-themes)
7. [Experiments against this gateway](#experiments-against-this-gateway)
8. [Recommendations for this project](#recommendations-for-this-project)
9. [Anti-patterns seen in the surveyed code](#anti-patterns-seen-in-the-surveyed-code)
10. [Open questions](#open-questions)
11. [Source index](#source-index)

## Scope and method

Each repository was shallow-cloned on 2026-11-01 and read at the commit below. Values in this report are code defaults at these commits. Documentation was used only where the code delegates to it (Envoy best-practice configuration).

| Project | Kind | Commit | Main files read |
| --- | --- | --- | --- |
| Go standard library (`net/http`, `httputil`) | Library | `52f1233d` (tip; local toolchain go1.26.10) | `httputil/reverseproxy.go`, `transport.go`, `server.go`, `internal/http2/config.go` |
| Caddy `reverse_proxy` | Server + proxy module | `9d2cb35` | `modules/caddyhttp/reverseproxy/{reverseproxy,httptransport,streaming}.go`, `modules/caddyhttp/{app,server,idletimeout}.go` |
| Traefik v3 | Edge router | `38d53dd` | `pkg/proxy/httputil/proxy.go`, `pkg/server/server_entrypoint_tcp.go`, `pkg/server/service/transport.go`, `pkg/middlewares/{forwardedheaders,retry,contenttype}`, `pkg/config/{static,dynamic}` |
| Skipper (Zalando) | Programmable proxy | `2237f1e` | `proxy/proxy.go`, `config/config.go`, `queuelistener/listener.go` |
| Lura (KrakenD core) | API aggregation gateway | `4c408dd` | `config/config.go`, `proxy/http.go`, `router/gin/endpoint.go`, `transport/http/{client,server}` |
| Tyk | API management gateway | `63988da` | `gateway/reverse_proxy.go`, `gateway/proxy_muxer.go`, `internal/httputil/streaming.go` |
| vulcand/oxy | Proxy middleware library | `56ba15b` | `buffer/buffer.go`, `connlimit/connlimit.go` |
| Envoy (C++, reference) | Edge/mesh proxy | `24a950b6` | `docs/root/configuration/best_practices/edge.rst` + `_include/edge.yaml`, `header_sanitizing.rst`, API protos |
| oauth2-proxy, Fabio | Specialized proxies | `bea3f04`, `09108b4` | Default options only |

Pomerium and Apache APISIX were cloned but not analysed. Their data planes are Envoy and NGINX/Lua, so they add little Go-specific proxy knowledge beyond the Envoy notes.

The [experiments](#experiments-against-this-gateway) ran against this repository's working tree, using a temporary module outside the repository.

## Summary

1. **Every Go project here builds on `httputil.ReverseProxy` or reimplements it line by line.** Caddy and Tyk carry forks; Traefik and this project use `Rewrite`; Skipper builds a fresh `http.Request`. They all keep the same core mechanics: clone the request, give a zero-length request a `nil` body, strip hop-by-hop headers while preserving `TE: trailers` and `Upgrade`, send an empty `User-Agent`, and abort with `http.ErrAbortHandler` after the response is committed. [ADR-0002](../adr/0002-use-go-standard-http-proxy.md) is consistent with the field.
2. **The mature projects harden the server edge *before* routing, not in the proxy.** Traefik wraps every entry point with path sanitizing, path normalizing, fragment, `CONNECT`, opaque-URI, and header-alias handling. Envoy recommends `normalize_path`, `merge_slashes`, a policy for escaped slashes, and rejecting headers with underscores. This gateway has none of these yet. Our experiments show that `/api/%2e%2e/admin` passes the `/api/` prefix and reaches the upstream as path `/api/../admin` ([E1](#e1-path-confusion)).
3. **Forwarding-header trust is an explicit list of trusted peers everywhere.** Caddy, Traefik, and Envoy discard client `X-Forwarded-*` unless the peer is in a trusted CIDR list. This project's "rebuild from the immediate peer" policy matches their untrusted default. Traefik also protects its managed headers from `Connection`-nomination and underscore aliases. Here, aliases such as `X_Forwarded_For` and `X_Real_IP` pass through ([E3](#e3-identity-header-aliases)).
4. **Slow-client defaults are converging on progress-based idle deadlines, not total deadlines.** Caddy now defaults to a 1-minute `ReadIdleTimeout`/`WriteIdleTimeout` that resets on every successful read or write, with optional minimum rates and a 64 KiB maximum write chunk. This directly addresses the slow-body item that [resource-limits.md](resource-limits.md) leaves open, without blanket deadlines. Traefik instead uses a 60s total `ReadTimeout` that also covers the body. Tyk uses 120s total read/write timeouts. Both models break long uploads or streams.
5. **Retries are safe only before the request reaches the upstream.** Skipper retries dial failures only, and only for bodyless requests. Traefik records `WroteHeaders`/`WroteRequest` through `httptrace` and buffers up to 2 MiB for status retries. Caddy retries dial errors on any method but other failures only for `GET` unless matchers say otherwise. No project retries after any response byte has been written to the client.
6. **Error-status mapping is standard:** 502 for an upstream failure, 504 for `net.Error.Timeout()`, and 499 for client cancellation (Caddy, Traefik). This gateway always renders 502.
7. **The transport defaults disagree widely**, but the projects agree on the need for a response-header timeout. Some disable it by default (Caddy, Traefik, Tyk core transport), which is risky. Others set one: Skipper 60s, Envoy route timeout 15s. All use a shared, long-lived transport, and some recycle connections periodically (Skipper closes idle connections every 20s; Tyk has `max_conn_time`).
8. **Bounding at the connection and stream level is an edge feature.** Skipper's queue listener limits accepted connections, with LIFO queueing based on memory. Envoy has listener connection limits and an overload manager, and recommends 100 HTTP/2 streams with 64 KiB/1 MiB flow-control windows at the edge. Go's default is 250 streams and 1 MiB windows.

## The canonical proxy request path

The diagram combines the phases that appear across the surveyed code. Each phase lists the decisions a gateway must make explicitly. The next column shows where this project currently decides them.

```text
  client
    │
 ①  accept ── connection limits, PROXY protocol, TLS handshake, ALPN (h1/h2/h2c)
    │
 ②  parse ─── request line + headers: header-read timeout, header size/count, h2 stream limits
    │
 ③  edge normalization ── reject/normalize: opaque URI, fragment, CONNECT, path dot-segments,
    │                    encoded slashes, query semicolons, aliasing header names
    │
 ④  client identity ── trusted-proxy check → keep or discard X-Forwarded-* / Forwarded / X-Real-IP
    │
 ⑤  route match ─── host/path/method; same normalized path as the upstream will see
    │
 ⑥  policy ──────── middleware: auth, limits, admission, body limits, request hooks
    │
 ⑦  outbound prep ─ clone; nil body when CL=0; strip hop-by-hop + Connection-nominated;
    │                keep TE:trailers / Upgrade; set XFF/XFH/XFP, Via; empty User-Agent;
    │                Host policy (pass or rewrite); path join; query handling
    │
 ⑧  upstream select ─ LB policy, health (active/passive), circuit breaker, egress allowlist
    │
 ⑨  transport ───── pool wait; dial timeout; TLS handshake + verification; h1/h2;
    │                write body (expect-continue); response-header timeout
    │
 ⑩  retry? ──────── only if no request bytes were sent (or body is replayable) AND
    │                nothing was written downstream
    │
 ⑪  response ────── 1xx forwarding; 101 → hijack + bidirectional copy; strip hop-by-hop;
    │                response hooks; copy headers; announce trailers; flush policy
    │                (SSE, CL=-1, h2 bidi); copy body with backpressure; trailers;
    │                abort with ErrAbortHandler on mid-stream error
    │
 ⑫  error mapping ─ 502 / 504 (timeout) / 499 (client gone) / 503 (no upstream / overload)
    │
 ⑬  completion ──── access log, metrics, tracing, outcome
```

| Phase | Go stdlib `ReverseProxy` | This gateway today |
| --- | --- | --- |
| ① accept | — | Plain `ListenAndServe` in the example |
| ② parse | `http.Server` (1 MiB headers, 500 header values, h2 250 streams by default) | 5s header-read timeout, 64 KiB headers ([main.go](../../cmd/gateway/main.go)) |
| ③ edge normalization | `ServeMux` cleans dot-segments in the *escaped* path only; `ReverseProxy` rejects `CONNECT` | Relies on `ServeMux` behavior ([E1](#e1-path-confusion)) |
| ④ client identity | `Rewrite` removes `Forwarded`/`X-Forwarded-*` before the callback | Rebuilds from the peer; drops `X-Real-IP` |
| ⑤ route match | `ServeMux` prefix | Prefix routes; one upstream per route |
| ⑥ policy | — | Middleware + request hooks; `admission`, `bodylimit` |
| ⑦ outbound prep | `removeHopByHopHeaders`, `cleanQueryParams`, `SetURL`, `SetXForwarded` | Uses stdlib defaults |
| ⑧ upstream select | — | Static; optional `egress` allowlist transport |
| ⑨ transport | `DefaultTransport`: 30s dial, 10s TLS, no response-header timeout, env proxy | Example: 3s/5s/10s, no env proxy, pool caps |
| ⑩ retry | None (only `http.Transport` internal retries for idempotent or nil-body requests on reused connections) | None |
| ⑪ response | 1xx via `httptrace.Got1xxResponse`; SSE and CL=-1 flush immediately; trailers; `ErrAbortHandler` | Stdlib + response hooks |
| ⑫ error mapping | `ErrorHandler` default = 502 | Always 502 |
| ⑬ completion | — | Completion hooks, `accesslog` |

### How Traefik orders the edge chain

Traefik's `createHTTPServer` (`pkg/server/server_entrypoint_tcp.go:660-800`) is the clearest real-world example of phase ③ and ④. Handlers are wrapped from inside out, so the request passes them in this order:

1. `populateRequestTLS` — fill in `req.TLS` from the connection when missing.
2. `denyOpaque` — 400 if `URL.Opaque != ""`. Go fills `Opaque` for request targets that are none of the four RFC 9112 forms. Routing ignores such a target, but `URL.RequestURI()` restores it when forwarding.
3. Alias headers — `keep` (default; a warning is logged at startup), `delete`, or `reject` for header names that contain anything other than letters, digits, and `-`.
4. `denyConnect` — 501 for `CONNECT` on every HTTP version.
5. `denyFragment` — 400 if the path contains `#`.
6. `normalizePath` — decode percent-encoded *unreserved* characters (`%2E` → `.`, `%41` → `A`) and uppercase the remaining encodings (RFC 3986 §6.2.2).
7. `sanitizePath` (default **on**) — `URL.JoinPath()` removes `.`/`..` segments and duplicate slashes, then rebuilds `RequestURI`.
8. Query semicolons — either `http.AllowQuerySemicolons` (`;` → `&`) or `encodeQuerySemicolons` (`;` → `%3B`).
9. `contenttype.DisableAutoDetection` — sets `Content-Type: nil` so `net/http` never sniffs a type for proxied bodies.
10. Optional keep-alive limits (maximum requests or maximum age per connection).
11. `forwardedheaders.XForwarded` — trust check, then rewrite `X-Forwarded-*`/`X-Real-Ip`, then remove `Connection`-nominated headers without removing managed X-headers.
12. Router → middlewares → service → proxy.

`normalizePath` runs before `sanitizePath`. Because of that order, `%2e%2e` becomes `..` and is then removed. This defeats the path confusion that [E1](#e1-path-confusion) shows here.

## Project notes

### Go standard library `httputil.ReverseProxy`

Source: `src/net/http/httputil/reverseproxy.go` (1010 lines).

- **Exactly one of `Rewrite` or `Director` must be set**, checked per request. `Director` is deprecated: hop-by-hop removal runs *after* it and can delete headers it added, and client `X-Forwarded-*` headers survive. With `Rewrite`, `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, and `X-Forwarded-Proto` are removed before the callback.
- **Cancellation:** it uses `req.Context()`. Only when the context has no `Done` channel does it fall back to `CloseNotifier`.
- **Body:** `req.Clone(ctx)`. If `ContentLength == 0`, the body becomes `nil` so `http.Transport` can retry (golang/go#16036). The outbound body is closed when the handler returns, because the transport goroutine can outlive the handler (golang/go#46866).
- **Hop-by-hop:** `Connection`-listed names, then `Connection`, `Proxy-Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`, `Transfer-Encoding`, `Upgrade`. It restores `Te: trailers` if the *inbound* request asked for trailers, and restores `Connection: Upgrade` + `Upgrade: <type>` for HTTP/1 upgrades. It rejects `Upgrade` on HTTP/2+ requests and does not forward `h2c` upgrades.
- **Query:** `cleanQueryParams` re-encodes the query when it contains `;`, an invalid `%` escape, or more parameters than the default limit. In practice, unparsable pairs are **dropped silently** ([E2](#e2-query-semicolons)).
- **`User-Agent`:** set to `""` when the client sent none, so Go's default UA is not added.
- **1xx:** forwarded through `httptrace.Got1xxResponse`, which restores middleware-set headers after each 1xx. A mutex stops writes after `RoundTrip` returns.
- **101:** the response upgrade type must be printable and equal to the requested type. It hijacks through `http.NewResponseController`, and request-context cancellation closes the backend connection (golang/go#35559).
- **Flush:** immediate (`-1`) for `text/event-stream` and for `ContentLength == -1`. Otherwise it uses `FlushInterval` (default 0 = no periodic flush).
- **Trailers:** builds the `Trailer` announcement from `res.Trailer`. It flushes to force chunked encoding when trailers exist, and uses `http.TrailerPrefix` for trailers that were not announced.
- **Mid-stream error:** `panic(http.ErrAbortHandler)` when running under `http.Server`, so the client sees a broken stream instead of a truncated success (golang/go#23643).

Relevant `net/http` defaults (code, not docs):

| Setting | Default |
| --- | --- |
| `DefaultTransport` | `ProxyFromEnvironment`, dial 30s, keepalive 30s, `ForceAttemptHTTP2`, `MaxIdleConns` 100, `IdleConnTimeout` 90s, TLS 10s, expect-continue 1s, **no `ResponseHeaderTimeout`** |
| `MaxIdleConnsPerHost` | 2 (`DefaultMaxIdleConnsPerHost`) |
| `Transport.MaxResponseHeaderBytes` | 10 MiB |
| `Server.MaxHeaderBytes` | 1 MiB |
| `Server.MaxHeaderValueCount` | 500 |
| HTTP/2 server streams | 250 per connection |
| HTTP/2 receive window | 1 MiB per connection and per stream (server) |
| HTTP/2 ping timeout | 15s |
| HTTP/2 queued control frames | 10000 |

### Caddy `reverse_proxy`

Caddy has the most complete Go proxy hardening of the projects surveyed. It forks `ReverseProxy` into `reverseproxy.go` (2397 lines), `streaming.go`, and `httptransport.go`.

**Request path** (`ServeHTTP` → `prepareRequest` → loop of `proxyLoopIteration` → `reverseProxy` → `finalizeResponse`):

- `prepareRequest` clones the request and applies optional rewrites. When a rewrite changes the method to `GET`/`HEAD`, the body is dropped. It wraps the body to carry request trailers, optionally buffers (`request_buffers`), and sets `nil` for CL=0, `Close = false`, and an empty `User-Agent`.
- `Early-Data: 1` is added when the TLS handshake is not yet complete (0-RTT, RFC 8470), so the upstream can refuse replayable requests.
- Hop-by-hop list adds **`Alt-Svc`** to the stdlib list. It removes `Connection`-nominated headers and keeps `Te: trailers`.
- **Forwarded headers** (`addForwardedHeaders`): the peer is trusted only if it is in `trusted_proxies` (default: none). Without trust, `X-Forwarded-For` is replaced with the peer IP, and `-Proto`/`-Host` are replaced with the connection's values. With trust, the prior XFF is appended and the *last* `-Proto`/`-Host` value is used. Unix-socket peers (`RemoteAddr == "@"`) without trust get all three removed. IPv6 zones are stripped before comparing CIDRs.
- `Via: <major>.<minor> Caddy` is appended to the request, and to the response in `finalizeResponse`. The upstream `Server` header is removed from the response.
- WebSocket over HTTP/2 or HTTP/3 extended `CONNECT` is translated to an HTTP/1.1 `Upgrade` with a fresh `Sec-WebSocket-Key`. WebSocket header casing is restored (`Sec-WebSocket-*`), and the WebSocket upstream connection is forced to HTTP/1.
- **Retries** (`tryAgain`): disabled unless `retries` or `try_duration` is set. Dial errors and "no upstream" are always retryable. Other transport errors are retried only for `GET`, unless `retry_match` matchers allow more. Buffered request bodies are replayed for each attempt. `try_interval` defaults to 250ms when `try_duration` is set.
- **Status mapping** (`statusError`): 502 by default, 504 for `net.Error.Timeout()`, and 499 for `context.Canceled`. A comment notes that the stdlib wraps the error, so a substring check is also needed. After exhausted response retries, the real upstream status is used.
- **Passive health**: `fail_duration`, `max_fails`, `unhealthy_status`, `unhealthy_latency`, `unhealthy_request_count`. A failure counts only for non-retryable-response errors.
- **Streaming**: `flush_interval` (default 0). Immediate flush for SSE, CL=-1, or HTTP/2 bidirectional streams. Caddy defines a bidirectional stream as both sides HTTP/2, CL=-1, and `Accept-Encoding` empty or `identity`, so an outer `encode` handler still works. There are also `stream_timeout` and `stream_close_delay` (which keeps WebSockets open across config reloads).

**Transport defaults** (`HTTPTransport.Provision`): versions `1.1, 2`; dial 3s; TCP keepalive probes 30s; `IdleConnTimeout` 2m; `MaxIdleConnsPerHost` 32 (per issue #2805); no `MaxConnsPerHost`; **no `ResponseHeaderTimeout`**; `ProxyFromEnvironment` (or a `network_proxy` module). Optional `read_timeout`/`write_timeout` set per-I/O TCP deadlines on upstream connections (`tcpRWTimeoutConn`). The TLS handshake timeout is applied through context, because a custom dialer is used.

**Server defaults** (`app.go:946-967`): `IdleTimeout` 5m; `ReadHeaderTimeout` 1m; **`ReadIdleTimeout` 1m and `WriteIdleTimeout` 1m**; no total `ReadTimeout`/`WriteTimeout`. The idle model (`idletimeout.go`, 651 lines):

- `IdleDeadline.next()` moves the deadline forward on each successful read or write. With `MinRate > 0`, the allowance grows from a fixed start at 1/MinRate seconds per byte, as in Apache `mod_reqtimeout`. This catches a client that trickles bytes just often enough to never go idle.
- An optional `HardDeadline` caps the result, so the idle model cannot extend an explicit `ReadTimeout`/`WriteTimeout`.
- `MaxWriteChunk` (default 64 KiB) splits large writes. Without it, one huge write would be covered by a single deadline, as with nginx `sendfile_max_chunk`.
- HTTP/1 uses connection deadlines through `http.ResponseController`. For HTTP/2, deadlines are cleared between reads and writes (`ClearBetweenReads`/`ClearBetweenWrites`) so one stream cannot affect others.
- A read deadline is also kept active while the handler writes (`readDeadlineWriter`).

Other server options: `strict_sni_host`, `client_ip_headers` with `trusted_proxies_strict` (right-to-left parsing), `enable_full_duplex` for HTTP/1, and `max_header_bytes`.

### Traefik v3

**Proxy** (`pkg/proxy/httputil/proxy.go`): a stdlib `ReverseProxy` with `Rewrite`, wrapped by an h2c upgrade handler. A "smart builder" selects a fasthttp-based proxy (`pkg/proxy/fast`) when the backend cannot use HTTP/2.

- `Rewrite` **copies the inbound `X-Forwarded-*`/`Forwarded` back** after the stdlib removed them. This is safe only because the entry-point `XForwarded` middleware has already sanitized them against `trustedIPs` (phase ④ before ⑦). The XFF append can be disabled per entry point (`notAppendXForwardedFor`).
- The path is taken from `RequestURI` (not `URL.Path`) so that the client's encoding is preserved. `;` in the query is replaced with `&`. `Opaque` is cleared, outbound trailer values are set to nil, and the outbound protocol is fixed to HTTP/1.1 (the transport upgrades as needed). `passHostHeader` decides whether to keep `Host`.
- `ComputeStatusCode`: `io.EOF` → 502, `context.Canceled` → 499, net timeout → 504, other `net.Error` → 502, everything else → 500.

**Forwarded headers** (`pkg/middlewares/forwardedheaders`):

- Untrusted peer: delete all managed X-headers (13 names, including `X-Forwarded-Port`, `-Prefix`, `-Uri`, `-Method`, `-Tls-Client-Cert`, `X-Real-Ip`).
- `isManagedXHeader` treats `_` as `-`, so `X_Forwarded_Proto` is also removed.
- `removeConnectionHeaders` **ignores managed X-headers in `Connection`**. This stops `Connection: X-Forwarded-For` from removing the header the gateway just set. It keeps an allowlist of names that may stay in `Connection`.
- `X-Forwarded-Proto` becomes `ws`/`wss` for WebSocket upgrades.

**Entry-point defaults** (`pkg/config/static`):

| Setting | Default |
| --- | --- |
| `ReadTimeout` | **60s** (the total request, *including body*) |
| `WriteTimeout` | 0 |
| `IdleTimeout` | 180s |
| `MaxHeaderBytes` / `MaxHeaderValueCount` | stdlib (1 MiB / 500) |
| HTTP/2 | 250 streams; 4096 HPACK encoder/decoder tables |
| `SanitizePath` | **true** |
| Encoded characters (`%2F`, `%5C`, `%00`, `%3B`, `%25`, `%3F`, `%23`) | all allowed by default; each can be rejected |
| `AliasHeadersStrategy` | unset = keep, with a startup warning recommending `delete`/`reject` |
| PROXY protocol header timeout | 200ms when `ReadHeaderTimeout` is 0 |

**Transport** (`createRoundTripper`): dial 30s, keepalive 30s, `IdleConnTimeout` 90s, TLS 10s, expect-continue 1s, **64 KiB read/write buffers**, `ProxyFromEnvironment`, no `ResponseHeaderTimeout` by default, and HTTP/2 health pings through `HTTP2Config.SendPingTimeout`/`PingTimeout` (ping timeout 15s). `MaxIdleConns` is deliberately left at 0. Flush interval default: 100ms.

**Retry middleware** (`pkg/middlewares/retry/retry.go`):

- Status-based retries apply only to idempotent methods (not `POST`, `PATCH`, `LOCK`) unless configured otherwise. The body is buffered up to **2 MiB** (`RetryDefaultMaxRequestBodyBytes`). Larger bodies turn off status retries and continue streaming from a peeked buffer.
- Network-error retries are armed only when the request reached the proxy layer (`proxyReached`). They stop once `httptrace` reports `WroteHeaders`/`WroteRequest`. A 401 from an earlier auth middleware is therefore never retried.
- No retry after the response writer has written or been hijacked. Exponential backoff uses the request context.

### Skipper

Skipper (`proxy/proxy.go`, 2147 lines) does **not** use `ReverseProxy`. `mapRequest` builds a new request with `http.NewRequestWithContext(method, u.String(), body)`, then copies headers. Hop-by-hop removal is a flag (`HopHeadersRemoval`) and is not always on. The empty `User-Agent` and CL=0 → `nil` body conventions are kept.

- **Retry**: `retryable()` requires a dial error, an LB backend, no 499, and `req.Body == nil || http.NoBody`. This is the most conservative policy surveyed.
- **Dial errors** are wrapped as `proxyError{dialingFailed: true}`, so retry logic does not need string checks. A context error after a successful dial maps to 504.
- **Loop protection**: `DefaultMaxLoopbacks = 9` for internal loopback routes.
- **Defaults** (`config/config.go`): server read 5m, read-header 60s, write 60s, idle 60s, `MaxHeaderBytes` 1 MiB; backend dial 60s, keepalive 30s, TLS 60s, response-header **60s**, expect-continue 30s, `MaxIdleConnsPerHost` 64, `CloseIdleConnsPeriod` **20s** (all idle connections are closed periodically, which picks up DNS and endpoint changes).
- **Queue listener** (`queuelistener/listener.go`): bounds accepted connections. Concurrency comes from a memory limit and expected bytes per connection (50 KiB per request by default), and the queue is 10× concurrency. Waiting connections are served **LIFO**, and the oldest is dropped when the queue is full. Under overload, recent clients get served and stale ones fail fast.
- Other features: passive health checks and endpoint fade-in, circuit breakers, rate limits, a WriteTimeout filter that sets a per-request write deadline through `ResponseController`, and FIFO/LIFO concurrency filters with queue timeouts.

### Lura / KrakenD

Lura is an *aggregating* gateway: by default it decodes backend responses and builds new ones. Its defaults are the most restrictive of the projects surveyed:

- **Header allowlist**: only `Content-Type` is forwarded (`server.HeadersToSend`) unless `input_headers` lists more. `"*"` forwards everything. `X-Forwarded-For` (from gin's `ClientIP()`) and `X-Forwarded-Host` are always set. The `User-Agent` is KrakenD's unless the client UA is allowlisted, in which case `X-Forwarded-Via` is added.
- **Query allowlist**: query parameters are dropped unless listed in `input_query_strings` (or `"*"`).
- **Timeout**: `DefaultTimeout = 2s` per endpoint, implemented with `context.WithTimeout` around the whole pipeline (`router/gin/endpoint.go:46`).
- **Status handling**: the default status handler treats every status except 200 and 201 as an error. Upstream error bodies are not passed to the client unless configured.
- **Transport**: `MaxIdleConnsPerHost` 250 by default, `ProxyFromEnvironment`.
- **TLS**: `MinVersion` defaults to **TLS 1.3** when not specified (`parseTLSVersion`), on both the server and client side.
- Redirects: POST bodies are buffered only if `send_body_on_redirect` is set.

This "no pass-through by default" model is the opposite of a transparent proxy. A route-level option for allowlisted pass-through would serve users who want KrakenD-style isolation.

### Tyk

Tyk keeps a heavily modified fork of `ReverseProxy` (`gateway/reverse_proxy.go`, 2329 lines). This is the growth risk that [ADR-0002](../adr/0002-use-go-standard-http-proxy.md) cites.

- **Body buffering**: `deepCopyBody` uses `io.ReadAll` on the full request body for every request that is not an upgrade or gRPC stream, so the body can be logged or replayed. Memory therefore grows with body size unless a separate size limit applies.
- **Transport**: per API, cached on the spec, and recreated after `max_conn_time` seconds (connection recycling). Dial timeout is the `proxy_default_timeout` (default **30s**), and the same value is the fallback headers timeout, implemented in a round-tripper decorator. `IdleConnTimeout` 90s, TLS 10s.
- **Enforced timeouts**: per-path `hard_timeout` and API-level global timeouts, implemented as round-tripper decorators.
- **TLS**: global and per-API `ssl_insecure_skip_verify` options. Common-name checking is implemented through `InsecureSkipVerify = true` plus a custom `VerifyPeerCertificate`. This pattern is easy to get wrong.
- **Server**: `ReadTimeout` and `WriteTimeout` both default to **120s** (`proxy_muxer.go`), which also limits long uploads and streams. WebSockets are off unless `enable_websockets` is set.
- In-memory loopback (`memconn`) is used for internal API-to-API calls.

### oxy (vulcand)

oxy is a library of `http.Handler` building blocks that Traefik v1 used.

- `buffer`: buffers the request (and optionally the response) to support retries with an expression language (`IsNetworkError() && Attempts() <= 2`). It keeps up to 1 MiB in memory, then spills to disk. **The default maximum body size is unlimited (`-1`)**, and the default maximum retry attempts is 10.
- `connlimit`: concurrent-request limit per source (`SourceExtractor`). It uses the same model as this project's [`admission`](../../extensions/admission/README.md), but keyed per client.
- `cbreaker`, `ratelimit`, `roundrobin` (with sticky sessions and a rebalancer), and `stream`.

### Envoy (reference for edge defaults)

Envoy's documentation says its defaults suit a service mesh and must be tightened at the edge. The recommended edge configuration (`edge.rst` + `edge.yaml`):

| Area | Recommendation |
| --- | --- |
| Buffers | `per_connection_buffer_limit_bytes` 32 KiB for listeners and clusters |
| Client IP | `use_remote_address: true`; do not consume external `X-Forwarded-*` |
| Paths | `normalize_path: true`, `merge_slashes: true`, explicit `path_with_escaped_slashes_action` (reject for RFC 3986 clients, unescape-and-redirect for browsers) |
| Headers | `headers_with_underscores_action: REJECT_REQUEST`; maximum request headers 60 KiB, 100 headers by default |
| HTTP/2 | 100 concurrent streams, 64 KiB initial stream window, 1 MiB connection window |
| Timeouts | connection idle 1h; `stream_idle_timeout` 300s; `request_timeout` 300s; route `timeout` 15s (the default) and route `idle_timeout` 15s; marked "disable for long-lived/streaming" |
| Connection limits | 10000 per listener; 50000 global; overload manager stops accepting requests at a heap threshold |
| Circuit breakers (defaults) | 1024 connections, 1024 pending, 1024 requests, 3 concurrent retries per cluster |

Envoy also **sanitizes its own control headers** (`x-envoy-*`, retry/timeout overrides, `x-request-id`, `x-forwarded-client-cert`) for requests it classifies as external. A gateway that reads control headers such as per-request timeouts or retry hints must strip them from untrusted clients.

### oauth2-proxy and Fabio

- oauth2-proxy upstreams: flush interval 1s, dial/response timeout 30s, `Host` passed by default for URL upstreams (but not for static upstreams), WebSockets on.
- Fabio: `MaxConn` 10000, dial 30s, flush 1s, `IdleConnTimeout` 15s.

## Default values compared

### Inbound server

| Project | Header read | Total read | Total write | Body idle | Idle conn | Max header bytes |
| --- | --- | --- | --- | --- | --- | --- |
| Go `http.Server` | 0 (= `ReadTimeout`) | 0 | 0 | — | 0 (= `ReadTimeout`) | 1 MiB |
| Caddy | 1m | 0 | 0 | **1m read / 1m write (progress-reset)** | 5m | 1 MiB |
| Traefik | — | **60s (includes body)** | 0 | — | 180s | 1 MiB |
| Skipper | 60s | 5m | 60s | — | 60s | 1 MiB |
| Tyk | — | 120s | 120s | — | — | stdlib |
| Envoy (edge rec.) | `request_headers_timeout` (optional) | `request_timeout` 300s | — | `stream_idle_timeout` 300s | 1h | 60 KiB |
| **This gateway (example)** | 5s | 0 | 0 | — | 60s | 64 KiB |

### Outbound transport

| Project | Dial | TLS | Response header | Idle conn | Idle/host | Max/host | Env proxy |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Go `DefaultTransport` | 30s | 10s | **none** | 90s | 2 | ∞ | yes |
| Caddy | 3s | (ctx) | **none** | 2m | 32 | ∞ | yes |
| Traefik | 30s | 10s | **none** | 90s | 2 (stdlib) | ∞ | yes |
| Skipper | 60s | 60s | 60s | 20s (forced close) | 64 | ∞ | via filter |
| Lura | config | config | config | config | 250 | — | yes |
| Tyk | 30s | 10s | 30s (decorator) | 90s | config | — | per-API |
| Envoy | 5s (cluster `connect_timeout` default) | — | route 15s | — | — | 1024 (breaker) | — |
| **This gateway (example)** | 3s | 5s | 10s | 90s | 16 | 64 | **no** |

This project's example transport is already stricter than most defaults. Disabling the environment proxy is unusual but deliberate ([egress](../../extensions/egress/README.md)).

### Streaming and retries

| Project | Flush default | Immediate flush | Retries default | Retry body |
| --- | --- | --- | --- | --- |
| Go | 0 | SSE, CL=-1 | none | — |
| Caddy | 0 | SSE, CL=-1, h2 bidi | off | replay of `request_buffers` |
| Traefik | 100ms | stdlib rules | middleware only | ≤ 2 MiB buffer |
| Skipper | — | own flusher | dial errors, bodyless only | none |
| oxy | — | — | expression-based | buffered, unlimited by default |
| Tyk | — | SSE detection | per-API | full in-memory copy |

## Hardening themes

### 1. Client identity and forwarding headers

The common pattern is a **trusted peer set** decided before routing:

- Untrusted peer: discard every identity header the gateway owns and set them from the connection (Caddy, Traefik, Envoy `use_remote_address`). This project does this for the four standard headers and `X-Real-IP`.
- Trusted peer: append to XFF and keep `-Proto`/`-Host` (Caddy uses the *last* value; Traefik keeps the existing value).
- Strict client-IP extraction parses XFF **right to left** and skips trusted hops (Caddy `trusted_proxies_strict`, Envoy `xff_num_trusted_hops`). Left-most-first is spoofable.

Gaps that the mature projects close and this gateway does not yet:

| Gap | Who handles it | Evidence here |
| --- | --- | --- |
| Header-name aliases (`X_Forwarded_For`, `X.Real.IP`) read as `HTTP_X_FORWARDED_FOR` by CGI/WSGI/PHP/nginx backends | Traefik (alias strategy, managed-header check), Envoy (reject underscores) | [E3](#e3-identity-header-aliases): passed through |
| Other forwarding headers: `X-Forwarded-Port`, `-Prefix`, `-Uri`, `-Method`, `-Server`, `X-Original-URL`, `X-Rewrite-URL`, `X-Forwarded-Tls-Client-Cert` | Traefik deletes its 13 managed headers for untrusted peers | [E3](#e3-identity-header-aliases): `X-Forwarded-Port` and `X-Original-URL` passed through |
| Gateway control headers (e.g. future per-request timeout/retry hints) | Envoy strips `x-envoy-*` from external requests | Not applicable yet |
| `Connection: X-Forwarded-For` removing gateway-set headers | Traefik ignores managed headers in `Connection` | Not affected: stdlib removes hop-by-hop headers *before* `Rewrite` sets XFF |

`X-Original-URL` and `X-Rewrite-URL` matter because some backend frameworks route by them. Through these headers, a client can bypass the gateway's path-based policy.

### 2. Path and query normalization (path confusion)

Path confusion happens when the gateway and the upstream decide on different paths. The gateway authorizes `/api/...` while the upstream serves `/admin`.

Go facts (verified in `server.go:2866` and [E1](#e1-path-confusion)):

- `ServeMux` matches and cleans the **escaped** path. Raw `..` and `//` produce a 307 redirect to the clean path, which is safe.
- Percent-encoded dot-segments (`%2e%2e`, `%2E%2E%2F`, `..%2f`) are **not** cleaned. They match the `/api/` prefix, and the upstream receives them unchanged. Go's server decodes them to `URL.Path = "/api/../admin"`. Any upstream that resolves dot-segments after decoding (common in frameworks and static servers) then serves `/admin`.
- A literal `#` in the request target is not a fragment for the server. It becomes part of the path and is forwarded as `%23`.
- `CONNECT` bypasses `ServeMux` canonicalization. `ReverseProxy` rejects `CONNECT`.

The projects use three strategies:

1. **Normalize, then route, then forward the normalized form** (Traefik: `normalizePath` + `sanitizePath`; Envoy: `normalize_path` + `merge_slashes`). This changes what the upstream receives; it fails for upstreams that need the exact encoding.
2. **Reject ambiguous encodings** (Traefik `encodedCharacters`: reject `%2F`, `%5C`, `%00`, `%3B`, `%25`, `%3F`, `%23`; Envoy `REJECT_REQUEST` for escaped slashes).
3. **Redirect to the canonical form** (Envoy `UNESCAPE_AND_REDIRECT`; Go `ServeMux` for raw dot-segments). This works for browsers, not for API clients.

Query semicolons: since Go 1.17, `;` is not a query separator. `ReverseProxy` then re-encodes the query and **drops unparsable pairs** ([E2](#e2-query-semicolons)). Traefik makes this an explicit choice: `;` → `&` by default, or `%3B`. Silent loss is the worst option: the client and the upstream may disagree on parameters, and the gateway has no record that it changed the request.

### 3. Hop-by-hop headers and protocol edges

| Item | Practice |
| --- | --- |
| Hop-by-hop list | Stdlib list everywhere. Caddy adds `Alt-Svc` (an upstream's HTTP/3 advertisement is not valid for the gateway's origin). |
| `TE: trailers` | Keep only the `trailers` token (stdlib, Caddy). |
| Upgrades | Copy the upgrade type, check that the response type matches, hijack, and close the backend connection when the context is cancelled. Caddy forces WebSocket upstreams to HTTP/1 and translates RFC 8441 extended `CONNECT`. |
| `h2c` upgrade | Not forwarded (stdlib). Traefik handles h2c at the entry point. |
| `CONNECT` | Rejected (stdlib 405, Traefik 501). |
| 0-RTT | Caddy sets `Early-Data: 1` (RFC 8470). |
| Content sniffing | Traefik disables `net/http` type sniffing for proxied responses. Here, `ReverseProxy` copies upstream headers before `WriteHeader`; sniffing applies only to local responses without `Content-Type`. |
| `Server`/`Via` | Caddy removes the upstream `Server` header and appends `Via` in both directions. `Via` helps loop detection between proxies. |
| Response decompression | If the client sent no `Accept-Encoding`, `http.Transport` adds `gzip` and decompresses transparently ([E3](#e3-identity-header-aliases) shows the added header). Caddy exposes this as `compression`. Decompression costs CPU and allows decompression-ratio amplification from a hostile upstream. |

### 4. Slow clients and timeouts

The projects show three models:

1. **Total deadlines** (`ReadTimeout`/`WriteTimeout`: Traefik 60s read, Tyk 120s, Skipper 5m/60s). These are simple but break legitimate long uploads and streams. On HTTP/2 a connection-wide deadline can affect unrelated streams. [resource-limits.md](resource-limits.md) already rejects this model.
2. **Progress-based idle deadlines** (Caddy `ReadIdleTimeout`/`WriteIdleTimeout`, Envoy `stream_idle_timeout`). These terminate only stalled transfers. Optional minimum rates catch slow trickles. A maximum write chunk lets the deadline apply to large writes. Per-stream handling on HTTP/2 clears deadlines between operations. This model matches the constraints in [resource-limits.md](resource-limits.md): it keeps healthy SSE, does not buffer, and uses no detached timers.
3. **Per-route request timeouts** (Lura 2s context, Envoy route 15s, Tyk hard timeouts). These suit RPC routes and must be disabled for streaming routes.

Caddy's implementation is the reference for a future `extensions/idletimeout` (or a serving-edge option). Its design decisions are reusable: deadline by `ResponseController`, `HardDeadline` cap, `MinRate` allowance from a fixed start, `MaxChunk` split, and clearing deadlines between HTTP/2 operations.

### 5. Request bodies, buffering, and retries

- **Default to streaming.** Caddy, Traefik, Skipper, and this project stream by default. Tyk's full-body copy and oxy's unlimited default buffer are the counterexamples.
- **Buffering is opt-in and bounded**: Caddy `request_buffers` and Traefik's 2 MiB retry buffer. When the limit is exceeded, they *fall back to streaming* (Traefik `peekedBody`) and do not fail.
- **Retry safety rule**, combined from the surveyed code: retry only if (a) the failure happened before request bytes were written (dial error, or `httptrace` shows no `WroteHeaders`), or the body is fully replayable, and (b) nothing was written downstream, and (c) the method is idempotent or explicitly allowed. Note that `http.Transport` already retries internally for some idempotent or nil-body requests on reused connections; the proxy's own retries come on top of that.
- **Typed dial errors** (Skipper `proxyError{dialingFailed}`, Caddy `DialError`) are better than string matching for retry decisions. This project's `egress` transport is a natural place to produce such a type.

### 6. Error mapping and client cancellation

| Condition | Caddy | Traefik | Here |
| --- | --- | --- | --- |
| Upstream connection/protocol error | 502 | 502 | 502 |
| Timeout (`net.Error.Timeout()`) | 504 | 504 | 502 |
| Client cancelled (`context.Canceled`) | 499 | 499 | 502 (not delivered) |
| No healthy upstream | 503 | 503 | n/a |
| Exhausted response retries | upstream status | upstream status | n/a |

499 has no effect for the client, who has already gone. It does correct metrics, access logs, and alerting, because client disconnects no longer count as upstream failures. [ADR-0009](../adr/0009-outcomes-errors-and-early-responses.md) already separates outcomes; the response status is the remaining part.

### 7. Connection management and upstream health

- **One long-lived transport per upstream policy**, never one per request. Tyk caches transports per API and recreates them after `max_conn_time`. Skipper closes idle connections every 20s. Both make sure DNS and endpoint changes take effect. [proxy-risk-follow-ups.md](proxy-risk-follow-ups.md) already notes that existing connections are not revalidated against new DNS answers.
- **HTTP/2 upstream health pings** (Traefik `SendPingTimeout`/`PingTimeout`) detect dead HTTP/2 connections that TCP keepalive misses.
- **Passive health** (Caddy, Skipper) and **circuit breakers** (Skipper, oxy, Envoy) belong in upstream selection ([ADR-0008](../adr/0008-separate-routing-from-upstream-selection.md)), not in routing.

### 8. Admission and edge capacity

- Request-level admission (this project's `admission`, oxy `connlimit`, Skipper FIFO/LIFO filters) does not limit accepted connections or idle HTTP/2 connections.
- Connection-level limits: Skipper's memory-based queue listener (LIFO, drops the oldest), Envoy listener limits and overload manager, Fabio `MaxConn`.
- HTTP/2 stream limits: Go defaults to 250 streams and 1 MiB windows. Envoy recommends 100 streams, a 64 KiB stream window, and a 1 MiB connection window at the edge. `http.Server.HTTP2` (`MaxConcurrentStreams`, `MaxReceiveBufferPerStream`, `MaxReceiveBufferPerConnection`) can set these without x/net.

### 9. TLS

- Lura defaults to a minimum of TLS 1.3 when no version is configured.
- `InsecureSkipVerify` switches exist in Tyk, Caddy, and Traefik. Tyk also uses `InsecureSkipVerify = true` together with custom verification for common-name checks. If verification is customized, keep `InsecureSkipVerify` false and use `VerifyConnection`.
- Caddy sets the TLS handshake timeout through the context because it uses a custom dialer. A custom `DialTLSContext` does not use `Transport.TLSHandshakeTimeout`. [egress](../../extensions/egress/README.md) already covers custom TLS dial paths.

## Experiments against this gateway

A temporary test module outside the repository (`replace example.com/go-gateway-starter => <worktree>`) built `gateway.New` with one `/api/` route and an `httptest` upstream that echoes what it receives. Raw request lines were written to a TCP connection to avoid client-side normalization. Toolchain: go1.26.10.

### E1: Path confusion

| Request target | Gateway result | Upstream `RequestURI` | Upstream `URL.Path` |
| --- | --- | --- | --- |
| `/api/../admin` | 307 → `/admin` | — | — |
| `/api//x` | 307 → `/api/x` | — | — |
| `/api/%2e%2e/admin` | **200 (proxied)** | `/api/%2e%2e/admin` | `/api/../admin` |
| `/api/..%2fadmin` | **200 (proxied)** | `/api/..%2fadmin` | `/api/../admin` |
| `/api/%2E%2E%2Fadmin` | **200 (proxied)** | `/api/%2E%2E%2Fadmin` | `/api/../admin` |
| `/api/x#frag` | 200 (proxied) | `/api/x%23frag` | `/api/x#frag` |
| `http://evil.example/api/x` (absolute form) | 200 (proxied) | `/api/x` | `/api/x` |

Raw dot-segments are safe. Encoded dot-segments reach the upstream and depend on how the upstream normalizes. This matters only when route prefixes enforce policy (for example authentication on `/api/` and none elsewhere) or when the upstream serves paths outside the route prefix. Absolute-form targets set `r.Host` to the target authority, which then becomes `X-Forwarded-Host`.

### E2: Query semicolons

| Request target | Upstream `RequestURI` |
| --- | --- |
| `/api/a;b?x=1;y=2` | `/api/a;b` — **the query is dropped** |

`cleanQueryParams` re-encodes through `url.ParseQuery`, which rejects `x=1;y=2`, so the whole pair set is lost. Neither the client nor the logs show this.

### E3: Identity header aliases

The request contained `X_Forwarded_For: 6.6.6.6`, `X-Forwarded-For: 7.7.7.7`, `X_Real_IP: 6.6.6.6`, `X-Forwarded-Port: 1`, `X-Original-URL: /admin`, and `Connection: close, X-Forwarded-For`. The upstream received:

```text
Accept-Encoding=gzip              ← added by http.Transport (transparent decompression)
X-Forwarded-For=127.0.0.1         ← correctly rebuilt; Connection-nomination had no effect
X-Forwarded-Host=gw
X-Forwarded-Port=1                ← client value passed through
X-Forwarded-Proto=http
X-Original-Url=/admin             ← client value passed through
X_forwarded_for=6.6.6.6           ← alias passed through
X_real_ip=6.6.6.6                 ← alias passed through
```

The four standard headers are handled correctly. Aliases and non-standard forwarding headers are not filtered. The README already says that "other vendor-specific identity headers are not comprehensively filtered". This experiment shows concrete examples that matter for CGI-style and URL-rewriting backends.

## Recommendations for this project

Ownership uses the categories from the README's [proxy safety boundaries](../../README.md#proxy-safety-boundaries). None of these require a change to the lifecycle hook model. Each candidate needs its own focused pass with positive and negative tests, as in [proxy-risk-follow-ups.md](proxy-risk-follow-ups.md).

### Priority 1 — correctness and confusion risks with evidence

| # | Candidate | Owner | Basis |
| --- | --- | --- | --- |
| R1 | **Path policy before routing.** Optional middleware with modes `reject` (default candidate for APIs: 400 for encoded `.`, `/`, `\`, NUL, and `#` in the path) and `normalize` (decode unreserved escapes, then clean, then route and forward the normalized form). Document that `ServeMux` cleaning alone is not enough. | Optional middleware, global position before routing; possible framework default later | E1; Traefik `normalizePath`/`sanitizePath`/`encodedCharacters`; Envoy edge guidance |
| R2 | **Explicit query-semicolon policy** instead of silent loss: `reject` (400), `encode` (`%3B`), or `split` (`&`, via `http.AllowQuerySemicolons`). Whatever the default, record the change. | Framework decision (it changes forwarded data) | E2; Traefik |
| R3 | **Reject or remove aliasing header names** (any name with characters outside `[A-Za-z0-9-]`), with `reject` as the recommended edge setting. | Optional middleware; candidate framework default | E3; Traefik alias strategy; Envoy `headers_with_underscores_action` |
| R4 | **Configurable list of untrusted identity headers** to remove before forwarding (start with `X-Forwarded-Port`, `-Prefix`, `-Uri`, `-Method`, `-Server`, `-Tls-Client-Cert*`, `X-Original-URL`, `X-Rewrite-URL`, `X-Client-IP`, `True-Client-IP`, `CF-Connecting-IP`). | Optional middleware or `Rewrite` extension | E3; Traefik managed X-headers |

### Priority 2 — operational accuracy

| # | Candidate | Owner | Basis |
| --- | --- | --- | --- |
| R5 | **Map proxy errors to 504 (timeout) and 499 (client cancelled)** in `renderError`; keep the generic body. Completion keeps the original error. | Framework | Caddy `statusError`, Traefik `ComputeStatusCode`; ADR-0009 |
| R6 | **Trusted-proxy mode** for the forwarding headers: CIDR list; append XFF and keep `-Proto`/`-Host` only from trusted peers; right-to-left client-IP extraction helper. The current "immediate peer" behavior stays the default. | Optional `Rewrite` policy | Caddy `trusted_proxies(_strict)`, Traefik `trustedIPs`, Envoy `xff_num_trusted_hops` |
| R7 | **Strip `Alt-Svc` from upstream responses**, and consider removing the upstream `Server` header. | Framework (hop-by-hop list) or response hook | Caddy hop list and `finalizeResponse` |
| R8 | **Example `http.Server.HTTP2` limits** (for example 100 streams, 64 KiB stream window, 1 MiB connection window) and documented `MaxHeaderValueCount`, when the example serves TLS/h2. | Example executable | Envoy edge guidance; Go defaults 250 / 1 MiB |

### Priority 3 — the open slow-body item

| # | Candidate | Owner | Basis |
| --- | --- | --- | --- |
| R9 | **Progress-based idle read/write deadlines** with an optional minimum rate, maximum write chunk, hard cap, and per-stream clearing on HTTP/2. This meets every condition [resource-limits.md](resource-limits.md) set for a generic idle-body wrapper: a defined workload, deadline ownership through `ResponseController`, reset rules, and no detached timers. Design and tests should follow Caddy's `idletimeout.go`. | Optional middleware or serving edge | Caddy defaults (1m/1m), Envoy `stream_idle_timeout` |
| R10 | **Typed dial/pre-write errors** from the transport (an `egress` dial-failure type plus an `httptrace` "request written" signal in `RequestContext`), so future retry or failover extensions can apply the safe-retry rule without string checks. No retry feature yet. | Framework context + `egress` | Skipper `proxyError`, Caddy `DialError`, Traefik `WroteHeaders` |
| R11 | **Periodic idle-connection retirement** in the example (e.g. `CloseIdleConnections` on an interval) or documentation of the DNS-change trade-off. | Example executable | Skipper 20s, Tyk `max_conn_time` |

### Keep as is

- **`Rewrite` with `SetXForwarded`** — the safest stdlib mode; Traefik uses the same API.
- **No `http.TimeoutHandler` or total deadlines** — supported by Caddy's move to idle deadlines and Envoy's "disable for streaming" notes.
- **No default request buffering** — the projects that buffer by default (Tyk, oxy) show the memory risk.
- **Disabled environment proxy and explicit response-header timeout in the example** — stricter than Caddy, Traefik, and the stdlib.
- **Not forking `ReverseProxy`** — Caddy and Tyk show what forking costs; the hook model covers what Traefik needs from the stdlib proxy.

## Anti-patterns seen in the surveyed code

| Anti-pattern | Where | Risk |
| --- | --- | --- |
| Total `ReadTimeout`/`WriteTimeout` as the default slow-client defense | Traefik (60s read), Tyk (120s/120s) | Kills legitimate uploads and streams; HTTP/2 cross-stream effects |
| Full in-memory request copy for every request | Tyk `deepCopyBody` | Memory grows with body size |
| Unlimited buffer default | oxy `DefaultMaxBodyBytes = -1` | Memory/disk exhaustion |
| No response-header timeout by default | Go `DefaultTransport`, Caddy, Traefik | Upstreams that hang hold slots until the client gives up |
| `InsecureSkipVerify = true` plus custom verification | Tyk common-name check | A verification bug silently accepts any certificate |
| Copying client `X-Forwarded-*` back in the proxy | Traefik `copyForwardedHeader` | Safe only because of an earlier middleware; unsafe if reused without it |
| Default alias strategy "keep" | Traefik (warns at startup) | Header spoofing through CGI-style backends |
| Error classification by string matching | Caddy `"operation was canceled"` (documented workaround) | Breaks when error text changes; prefer `errors.Is`/typed errors |

## Open questions

1. Should R1–R3 be **framework defaults** or **optional middleware**? Traefik makes path sanitizing a default but alias handling opt-in; Envoy makes everything opt-in for compatibility. The [scope ADR](../adr/0001-framework-scope.md) suggests opt-in, but these are proxy-correctness issues rather than business features.
2. For R1 `normalize` mode, does the upstream receive the normalized path (Traefik, Envoy) or the original encoding? Forwarding the original keeps the confusion risk.
3. Should the example executable serve TLS/h2 so that the HTTP/2 limits (R8) and idle-deadline behavior per stream (R9) are exercised by default?
4. If retries are added later, which layer owns body replay: an optional bounded buffer middleware (Traefik model) or the upstream selector (Caddy model)?

## Source index

Paths are relative to each repository at the commits in [Scope and method](#scope-and-method).

| Topic | File:line |
| --- | --- |
| Stdlib ServeHTTP, CONNECT, User-Agent, 1xx, trailers, abort | `go/src/net/http/httputil/reverseproxy.go:421-760` |
| Stdlib hop-by-hop list | `go/src/net/http/httputil/reverseproxy.go:382` |
| Stdlib query re-encoding | `go/src/net/http/httputil/reverseproxy.go:971` |
| Stdlib upgrade handling | `go/src/net/http/httputil/reverseproxy.go:860` |
| `DefaultTransport` | `go/src/net/http/transport.go:46` |
| `ServeMux` escaped-path cleaning | `go/src/net/http/server.go:2866` |
| HTTP/2 server defaults | `go/src/net/http/internal/http2/server.go:53`, `config.go:40-70` |
| Caddy request prep and forwarded headers | `caddy/modules/caddyhttp/reverseproxy/reverseproxy.go:627-1180` |
| Caddy retry policy | `reverseproxy.go:1547` |
| Caddy status mapping | `reverseproxy.go:1907` |
| Caddy transport defaults | `reverseproxy/httptransport.go:185-500` |
| Caddy server defaults | `caddy/modules/caddyhttp/app.go:946-967` |
| Caddy idle deadlines | `caddy/modules/caddyhttp/idletimeout.go`, `server.go:640-690` |
| Traefik proxy rewrite and status mapping | `traefik/pkg/proxy/httputil/proxy.go` |
| Traefik edge chain | `traefik/pkg/server/server_entrypoint_tcp.go:660-1020` |
| Traefik entry-point defaults | `traefik/pkg/config/static/entrypoints.go:115-154`, `static_config.go:47-51` |
| Traefik transport | `traefik/pkg/server/service/transport.go:304` |
| Traefik forwarded headers | `traefik/pkg/middlewares/forwardedheaders/forwarded_header.go` |
| Traefik retry | `traefik/pkg/middlewares/retry/retry.go:50-260` |
| Skipper request mapping, dialer, retryable | `skipper/proxy/proxy.go:673-800, 1573` |
| Skipper defaults | `skipper/proxy/proxy.go:55-75`, `skipper/config/config.go:739-759` |
| Skipper queue listener | `skipper/queuelistener/listener.go:174-190` |
| Lura header/query allowlists | `lura/router/gin/endpoint.go:123-175`, `lura/transport/http/server/server.go:49` |
| Lura defaults and TLS | `lura/config/config.go:32-35`, `lura/transport/http/server/tls.go:279` |
| Tyk transport, timeouts, body copy | `tyk/gateway/reverse_proxy.go:499, 719, 2194` |
| Tyk server timeouts | `tyk/gateway/proxy_muxer.go:455-490` |
| oxy buffer defaults | `oxy/buffer/buffer.go:49-55` |
| Envoy edge guidance | `envoy/docs/root/configuration/best_practices/edge.rst`, `_include/edge.yaml` |
| Envoy header sanitizing | `envoy/docs/root/configuration/http/http_conn_man/header_sanitizing.rst` |
| Envoy defaults | `envoy/api/envoy/config/cluster/v3/circuit_breaker.proto:78-92`, `config/route/v3/route_components.proto:1414`, `config/core/v3/protocol.proto:359-368` |
