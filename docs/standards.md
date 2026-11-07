# Standards

**Status:** Index of the RFCs that the core uses, and of the deliberate deviations from them. It describes the code as it is. The details are in the linked GoDoc and ADRs; this document does not repeat them.

The project runs no external conformance suite. "Implemented" means that the parts in the _Scope_ column are implemented and tested, not that the gateway is compliant with the complete RFC. Behavior that `net/http` provides, such as message parsing, is listed as _Delegated_.

Update this document when a change adds a protocol behavior, a new RFC reference, or a deviation (see [AGENTS.md](../AGENTS.md)).

## HTTP

| RFC | Title | Status | Scope | Where |
| --- | --- | --- | --- | --- |
| [9110](https://www.rfc-editor.org/rfc/rfc9110) | HTTP Semantics | Implemented | Hop-by-hop fields and `Connection` options in headers and declared or late request trailers (7.6.1), upgrades (7.8), trailer fields and the removal of the fields that a trailer must not contain (6.5.1; the list is in the `Gateway` GoDoc), status of failures (15), 1xx responses | `Gateway` GoDoc in [gateway.go](../gateway.go); ADR-0017, ADR-0019, ADR-0023, ADR-0027 |
| [9112](https://www.rfc-editor.org/rfc/rfc9112) | HTTP/1.1 | Delegated, with checks | Message parsing and framing by `net/http`. The core checks the request target (3.2), also for an unescaped `#` (a fragment) anywhere in the target, which `net/http` accepts; `Host` for the absolute form (3.2.2); and chunked trailers (7.1.2) | `Gateway` GoDoc; ADR-0026, ADR-0027 |
| [9113](https://www.rfc-editor.org/rfc/rfc9113) | HTTP/2 | Delegated | Protocol by `net/http`. The `Server` sets the stream limit and the flow-control windows (6.5.2, 6.9.2) | `Server` GoDoc in [server.go](../server.go); ADR-0024 |
| [7541](https://www.rfc-editor.org/rfc/rfc7541) | HPACK | Delegated | HTTP/2 field compression by `net/http`; the retry test fixture uses the static `:status 204` entry (Appendix A) | `request_buffer_retry_test.go`; ADR-0031 |
| [9111](https://www.rfc-editor.org/rfc/rfc9111) | HTTP Caching | Not implemented | The gateway is not a cache | ADR-0011 |
| [9114](https://www.rfc-editor.org/rfc/rfc9114) | HTTP/3 | Not supported | The core removes `Alt-Svc`, so clients do not switch to it through the gateway | ADR-0012 (R7) |
| [6455](https://www.rfc-editor.org/rfc/rfc6455) | The WebSocket Protocol | Implemented | Forwarding of the HTTP/1.1 upgrade (101) and the idle deadlines of upgraded connections | ADR-0019, ADR-0023, ADR-0024 |
| [8441](https://www.rfc-editor.org/rfc/rfc8441) | WebSockets over HTTP/2 | Not supported | Extended `CONNECT` is rejected with all `CONNECT` requests | `Gateway` GoDoc |
| [7239](https://www.rfc-editor.org/rfc/rfc7239) | Forwarded HTTP Extension | Partial | The core removes `Forwarded` from inbound requests and does not generate it. It sets the de-facto `X-Forwarded-For`, `-Host`, and `-Proto` headers from the direct peer, which no RFC defines | `Gateway` GoDoc; ADR-0012 (R4) |
| [7838](https://www.rfc-editor.org/rfc/rfc7838) | HTTP Alternative Services | Implemented (removal) | `Alt-Svc` from the upstream is removed from responses, 1xx responses, and trailers, because it is not valid for the origin of the gateway | ADR-0012 (R7), ADR-0019 |
| [9457](https://www.rfc-editor.org/rfc/rfc9457) | Problem Details for HTTP APIs | Implemented | Default body of gateway responses, with the extension member `reason` (3.2) | `Renderer` GoDoc in [render.go](../render.go); ADR-0022 |
| [6585](https://www.rfc-editor.org/rfc/rfc6585) | Additional HTTP Status Codes | Implemented | 431 for request headers over the limit (by `net/http`); 429 is available to extensions through a `Rejection` | `Server` GoDoc; ADR-0022 |

## URIs and hosts

| RFC | Title | Status | Scope | Where |
| --- | --- | --- | --- | --- |
| [3986](https://www.rfc-editor.org/rfc/rfc3986) | URI Generic Syntax | Implemented | Path policy (dot-segments, percent-encoding), query check, origins of the egress allowlist | `Gateway` GoDoc; ADR-0012 (R1, R2), ADR-0025 |
| [6874](https://www.rfc-editor.org/rfc/rfc6874) | IPv6 Zone IDs in URIs | Rejected | Zones are not permitted in upstream hosts, absolute-form authorities, or egress origins, because the transport removes them | `UpstreamSelector` GoDoc in [upstream.go](../upstream.go); ADR-0019 |
| [5890](https://www.rfc-editor.org/rfc/rfc5890), [3492](https://www.rfc-editor.org/rfc/rfc3492) | IDNA, Punycode | Required form | Upstream hosts and egress origins must use the ASCII (A-label) form | ADR-0019, ADR-0025 |
| [5952](https://www.rfc-editor.org/rfc/rfc5952) | IPv6 Address Text Representation | Implemented | Canonical form for the comparison of egress origins | `EgressPolicy` GoDoc in [egress.go](../egress.go) |
| [WHATWG URL](https://url.spec.whatwg.org/#ends-in-a-number-checker) | URL Standard, "ends in a number" | Implemented (egress) | An egress origin whose host ends in a number, such as `127.1` or `0x7f.1`, is rejected, because URL parsers and `inet_aton` read it as an IPv4 address. This is not an RFC | `EgressPolicy` GoDoc in [egress.go](../egress.go) |

## Networks and TLS

| RFC | Title | Status | Scope | Where |
| --- | --- | --- | --- | --- |
| [6890](https://www.rfc-editor.org/rfc/rfc6890), [1918](https://www.rfc-editor.org/rfc/rfc1918), [4291](https://www.rfc-editor.org/rfc/rfc4291) | Special-purpose, private, and IPv6 addresses | Implemented (egress) | The egress allowlist has no implicit permissions for these ranges. Unspecified, multicast, and scoped addresses are always denied | `EgressPolicy` GoDoc; ADR-0025 |
| [8446](https://www.rfc-editor.org/rfc/rfc8446), [6066](https://www.rfc-editor.org/rfc/rfc6066), [7301](https://www.rfc-editor.org/rfc/rfc7301) | TLS 1.3, SNI, ALPN | Delegated | `crypto/tls` from the caller's configuration. The `Server` adds the ALPN protocols that it needs; the egress transport sends the request host as SNI | `Server` and `EgressTransport` GoDoc; ADR-0016, ADR-0025 |

## Bundled extensions

| RFC | Title | Status | Scope | Where |
| --- | --- | --- | --- | --- |
| [2104](https://www.rfc-editor.org/rfc/rfc2104) | HMAC | Implemented (demonstration) | `X-Gateway-Signature` is HMAC-SHA256 over the method, the outbound scheme and authority, `Host`, and the escaped request URI. It is not an authentication protocol: no timestamp, replay protection, or body integrity. The fields are joined with newlines, and the hook fails when a field other than the last contains one, so the signed string is unambiguous. `WithKey` requires a key of at least 32 bytes, the output size of SHA-256 (section 3) | `outboundsign` package GoDoc |

`X-Request-ID` of the access log is a common header that no RFC defines. The access log generates its value and never accepts the client value ([ADR-0029](adr/0029-access-log-extension-contract.md)).

## Deliberate deviations

Each deviation is documented where users see it. This list only points to it.

| RFC | Section | Deviation | Default | Relaxation | Where |
| --- | --- | --- | --- | --- | --- |
| 9112 | 3.2.2 | The absolute form is rejected | Reject | `Config.AllowAbsoluteForm` | `Gateway` GoDoc, "Deviations from HTTP specifications"; ADR-0026 |
| 9110 | 5.1 | Header and trailer names with token characters other than letters, digits, and `-` are rejected | Reject | `HeaderNameDrop` | `Gateway` GoDoc; ADR-0012 (R3), ADR-0026 |
| 3986 | 3.4 | Queries with `;` or other input that `url.ParseQuery` rejects are rejected | Reject | `QueryUnchecked` | `Gateway` GoDoc; ADR-0012 (R2), ADR-0026 |
| 9110 | 15 | 499 is sent for a client that is gone; it is not a registered status | – | – | `Gateway` GoDoc; ADR-0023 |

## Request-body processing

[ADR-0031](adr/0031-route-first-request-bodies.md) fixes the matched route before body processing. Body limits count bytes exposed by the HTTP body reader, excluding transfer framing (RFC 9112). Streaming is the default; explicit memory buffering does not buffer responses. Excessive content gives 413 (RFC 9110, section 15.5.14), established inbound buffering deadlines give 408 (15.5.9), and storage reservation exhaustion gives 503 (15.6.4). Unknown reader failures, including untyped chunk-decoder or HTTP/2 length-check errors without classified evidence, give 500, not an inferred client-input status. The existing cancellation and commitment rules remain in force. When the gateway forwards unread buffered content of unknown length without trailer fields, it replaces the inbound chunked transfer coding with a Content-Length (RFC 9110, section 8.6). Transfer coding is a property of one connection (RFC 9112, section 6.1), so the content does not change. Trailer fields keep the chunked coding (RFC 9112, section 7.1.2), and an empty body of unknown length is not changed to length zero. No new protocol syntax deviation is introduced.

## Open items

- **Conformance suite.** No external suite, such as h2spec for HTTP/2, verifies the delegated parts in the gateway configuration.
