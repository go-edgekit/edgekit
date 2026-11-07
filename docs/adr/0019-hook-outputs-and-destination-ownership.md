# ADR-0019: Own the destination structurally and sanitize after hooks

- **Status:** Accepted; path/query rewrite configuration and available-trailer checks superseded in part by [ADR-0031](0031-route-first-request-bodies.md)
- **Date:** 2026-11-03
- **Decision owners:** Project maintainers
- **Amends:** [ADR-0003](0003-proxy-lifecycle-hooks.md) (the `BeforeUpstream` capability)

## Context

ADR-0008 makes upstream selection own the destination. ADR-0003 gives `BeforeUpstream` hooks the actual rewritten outbound request. A hook that gets a mutable `*http.Request` can change `URL` and send the request to a different destination than the selected upstream. The selected-upstream fact is then wrong, and egress checks have to examine a destination that no selector chose. Checks of scheme and host after the hooks do not cover changes to the path, the query, or the body.

Pingora separates the choice of the upstream peer from the callback that changes upstream request headers. Go's `ReverseProxy.Rewrite` deliberately permits destination changes, which is why the core does not give hooks a `Rewrite`-style request.

Hooks can also add header-map entries that undo core protections. Probes on Go 1.26 showed:

- `ReverseProxy` removes hop-by-hop fields before `Rewrite` and before `ModifyResponse`, and does not repeat the removal after them. For a 101 response it does not remove them at all, and it writes the header map raw after the hijack.
- On HTTP/1.1, the transport writes a hook's `Connection: X-Foo` without changes, so the next hop removes `X-Foo`. On HTTP/2 the same entry fails the round trip, which looks like an upstream failure.
- Both transports forward the header name `X_User`, which some upstreams read as `X-User` (R3). Both fail the round trip for a value with a control character.
- The HTTP/1.1 request writer ignores `Host`, `Content-Length`, `Transfer-Encoding`, and `Trailer` in the header map. HTTP/2 ignores them or rejects them.
- The server ignores an invalid `Content-Length` that a handler sets, uses chunked encoding, and writes the value to its error log. It accepts only a value that `strconv.ParseInt` reads as a non-negative `int64`. `ReverseProxy` adds header entries of all letter cases under the canonical name, so a `content-length` entry reaches the server too.
- Both transports normalize the `Host` value: they convert a non-ASCII name to Punycode, the HTTP/1.1 writer removes an IPv6 zone (RFC 6874), and a value with other invalid characters is sent empty or fails the round trip.
- `ReverseProxy` chooses the path for a 101 response before it calls `ModifyResponse`.

## Decision

### Destination ownership is structural

`BeforeUpstream` hooks get an `OutboundRequest`, not an `*http.Request`. It gives read access to the method, a copy of the rewritten URL, the `Host` value, and the content length. Its only mutable part is the outbound header map. It is valid only during the hook call.

The type, not a check after mutation, enforces that a hook cannot change the destination (scheme and authority), the path, the query, the method, `Host`, the body, the context, or the trailers. Signing hooks see the real rewritten request.

The upstream selector contract accepts only a host that the transport sends without changes: ASCII letters, digits, `.`, `-`, and `_`, or an IPv6 literal in brackets, with an optional port. An internationalized name must be given in its Punycode form, and IPv6 zones are not supported. So `OutboundRequest.Host` is the value that the upstream receives, and a signature over it agrees with the request on the wire. A selector result that breaks this rule is a selection failure (500, ADR-0017). A capability for the body, for example for a body hash, needs its own decision. Egress checks apply at dial time, to the address that the transport connects to.

### Two kinds of header fields

- **Generated framing.** The HTTP implementation writes these fields from structured values that hooks cannot reach: `Host` from the request host, request `Content-Length` from the content length, chunked `Transfer-Encoding` and `Connection: close` from the server or transport, and the `Trailer` announcement from the trailer map. No list below removes framing that the core or the transport generates.
- **Forbidden header-map entries.** These are entries that the core removes or restores, per message, after the hooks of that message ran. This is normal enforcement, not a failure.

### Outbound request, after middleware and `BeforeUpstream` hooks

The check runs for each proxied request, also when no `BeforeUpstream` hook is configured, because middleware can set the same entries. It covers the complete outbound header map and the names in the outbound trailer map. A protection that applies only when an unrelated hook is configured is not a protection: middleware that makes a header name from client data is the attacker-controlled path that R3 is for.

| Fields | Rule |
| --- | --- |
| `Connection`, `Upgrade`, `Te`, `Keep-Alive`, `Proxy-Connection`, `Proxy-Authorization` | Restored to their values before the hooks. Usually they are absent. For an upgrade request they are `Connection: Upgrade` and the `Upgrade` value. If the client accepts trailers, `Te` is `trailers`. |
| `Host`, `Content-Length`, `Transfer-Encoding`, `Trailer` | Removed from the header map. |
| Identity headers and managed headers | Kept. The owning extension sets them. Because `Connection` is restored, no token can name them. |
| A header or trailer name with characters other than ASCII letters, digits, and `-` (R3); a header value with a control character other than horizontal tab | A failure in the `BeforeUpstream` phase. The core does not call the transport, sends 500 without details (ADR-0017), and runs completion after middleware unwinds. The error tells a name failure from a value failure. Diagnostics do not contain the name or the value. |

The trailer values arrive while the body streams, after the transport started, so the core cannot check them. The `BeforeUpstream` phase is a stage: the outbound request after middleware and `BeforeUpstream` hooks, before the transport. So it also records an invalid entry that middleware set when no hook is configured.

A silent removal of an invalid entry would hide a defect in the extension and could forward a request without a required signature or identity header.

### Final response, after `UpstreamResponse` hooks (not 101)

| Fields | Rule |
| --- | --- |
| `Connection` and each field that it names; `Keep-Alive`, `Proxy-Connection`, `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`, `Transfer-Encoding`, `Upgrade`; `Alt-Svc` (R7) | Removed. |
| `Content-Length` | The entries of all letter cases are taken together. The field is kept, under the canonical name, when they give exactly one value of decimal digits that fits in a non-negative `int64`, the range that the server accepts. Otherwise all entries are removed, and the server chooses the framing. A hook that changes the body must update the field or delete it. |
| Trailer names in `Response.Trailer` | The names above, `Content-Length`, and `Host` are removed (RFC 9110, section 6.5.1). The values that arrive later for removed names are not sent. |

### 101 Switching Protocols, after `UpstreamResponse` hooks

| Fields | Rule |
| --- | --- |
| `Upgrade` | Restored to the upstream's values under the canonical name. Entries in other letter cases are removed. |
| `Connection` | Restored as the canonical `Connection: Upgrade` when the upstream's `Connection` contained the `Upgrade` token. Otherwise it is removed, so the upgrade validation of `ReverseProxy` rejects the response of the upstream. |
| Each field that `Connection` names, except `Upgrade`; `Keep-Alive`, `Proxy-Connection`, `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`; `Alt-Svc` | Removed. |
| `Content-Length`, `Transfer-Encoding` | Removed. A 1xx response must not contain them (RFC 9110, section 8.6; RFC 9112, section 6.1). |

The core reads the tokens of all `Connection` entries, in all letter cases, before it removes any field, so the result does not depend on the iteration order of the header map.

`Upgrade` and the `Upgrade` token in `Connection` are the protocol switch. The core owns them as it owns the outbound hop-by-hop fields: it saves them before the hooks and restores them after the hooks, so the upgrade validation of `ReverseProxy` checks what the upstream sent. A hook cannot break or change the switch, and an upstream that does not accept the switch is still a failure without a known source (ADR-0017). `ReverseProxy` validates only the canonical `Upgrade` name but copies all letter cases to the client, so the restore also makes sure that the client gets only the validated value.

### Response status after `UpstreamResponse` hooks

A hook can change a final status to another status from 200 to 599. It must not change a 101 response to another status, or another response to 101 or to another status below 200, because `ReverseProxy` chose the upgrade path before the hooks. Such a change is a failure in the `UpstreamResponse` phase: the core closes the upstream body and sends 500 (ADR-0017).

The response hooks get their context and the gateway context from the outbound request that the core sent, not from the `Request` field that a transport sets. The core sets `Response.Request` to that request.

Informational responses other than 101 do not reach hooks. The core removes `Alt-Svc`, the hop-by-hop fields, the fields that `Connection` names, `Content-Length`, and `Transfer-Encoding` from them, and does not send the response headers that middleware set before the upstream responded.

## Consequences

**Positive:** Extension mistakes cannot change the destination or undo the hop-by-hop, `Alt-Svc`, and header-name protections. The selected-upstream fact agrees with the executed request. A defective header from a hook or from middleware is a 500 with a known phase, not a 502 from the transport, and the result does not change when a hook is added or removed.

**Negative:** Hooks cannot change the URL, even for legitimate needs. A path or query rewrite belongs to the route configuration. The outbound check is one pass over the header map for each proxied request, also without hooks; `BenchmarkServeHTTP` measures its cost. Middleware that sets a header name outside R3 makes the request fail with 500.

## Alternatives considered

- **Mutable `*http.Request` with checks after the hooks:** Checks of scheme and host miss changes to the path, the query, and the body, and a check cannot detect a consumed body.
- **Documentation only:** It leaves the selected-upstream fact inconsistent with the executed request.
- **Silent removal of invalid outbound headers:** It hides defects and can forward a request without a required header.
- **The outbound check only when a `BeforeUpstream` hook is configured:** No cost without hooks, but an unrelated hook, such as a tracing hook, changes the result for the same request, and R3 does not cover the headers that middleware sets.
- **A check of only the entries that the hooks changed:** A snapshot and a comparison cost more than a check of the complete map, and R3 does not cover middleware.
- **A check only when middleware passed on a different header map:** A security check would then depend on middleware that obeys the contract of `Middleware`.

## Validation / follow-up

Tests verify each row of the tables above, including the 101 response and its upgrade validation, trailer names, informational responses, and the restored fields of an upgrade request.

**Amended 2026-11-05:** [ADR-0027](0027-request-trailers-at-the-end-of-the-body.md) replaces the sentence "The trailer values arrive while the body streams, after the transport started, so the core cannot check them" for client trailers: the core checks the trailers that arrive with the body at its end, and forwards the allowed values.
