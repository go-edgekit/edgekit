# ADR-0026: Relax the request secure defaults explicitly

- **Status:** Accepted
- **Date:** 2026-11-05
- **Decision owners:** Project maintainers
- **Refines:** [ADR-0012](0012-secure-defaults-and-configurable-protections.md) (relaxation of the request secure defaults)

## Context

ADR-0012 says that users may deliberately relax a secure default, and that a relaxation is never the result of an omitted value. The request secure defaults run before routing (ADR-0018): the rejection of `CONNECT`, of request targets that are not in origin form, and of `Upgrade` values that are not printable; the path policy in reject mode (R1); the query check (R2); and the header-name check (R3).

Not all of these can be relaxed safely:

- Middleware and identity-header removal match canonical header names. If the gateway forwarded a name such as `X_Forwarded_For`, an upstream that reads `_` as `-`, such as a CGI-style server, would get a spoofed identity header. The outbound header check of [ADR-0019](0019-hook-outputs-and-destination-ownership.md) also rejects such a name, so a forwarded client header would cause a 500.
- `ReverseProxy` cannot forward `CONNECT`, the asterisk form has no path to route, and `ReverseProxy` refuses an `Upgrade` value that is not printable.
- A relaxation of the path policy forwards an encoding that the router did not match. Its meaning depends on normalize mode and on path rewriting, which have their own decision.
- RFC 9112, section 3.2.2, says that a server must accept the absolute form. In the absolute form, the server uses the authority of the request target and ignores the `Host` header field. `net/http` does this.
- `ReverseProxy` removes query parameters that `url.ParseQuery` rejects. The core restores the raw query of the inbound request in its rewrite step.

## Decision

Three relaxations exist. Each one is a gateway-wide configuration field whose zero value is the secure default. The checks run before routing, so a route cannot relax them.

| Field | Values | Effect |
| --- | --- | --- |
| `Config.QueryPolicy` | `QueryReject` (zero), `QueryUnchecked` | `QueryUnchecked` accepts each query and forwards the raw query unchanged. Middleware that calls `URL.Query` can then see other parameters than the upstream. |
| `Config.HeaderNamePolicy` | `HeaderNameReject` (zero), `HeaderNameDrop` | `HeaderNameDrop` removes each header and trailer name with characters other than ASCII letters, digits, and `-`, in the same step as the identity and managed headers, before any middleware runs, and continues. |
| `Config.AllowAbsoluteForm` | `false` (zero), `true` | `true` accepts an absolute-form request target with the scheme `http` or `https`, a host, and no user information or fragment. An unescaped `#` anywhere in the target is a fragment, also where `url.ParseRequestURI` keeps it in the path or the raw query; `%23` is not. |

For an accepted absolute-form target, the gateway handles the request as the origin-form request with the same path and query, and with `Host` set to the authority of the target (RFC 9112, section 3.2.2). Middleware, hooks, and the upstream selector get a URL without scheme and host. An empty path becomes `/`. The authority must be in the form that the upstream-selector contract accepts for a host. The authority goes to the upstream only in `X-Forwarded-Host`. It never selects the upstream, except through a selector that reads `Host`, and it never changes the destination that the egress transport checks. `RequestURI` keeps the value that the client sent.

These checks stay mandatory: the rejection of `CONNECT`, of the authority and asterisk forms and opaque targets, of `Upgrade` values that are not printable, the path policy, the outbound header check of ADR-0019, and the removal of identity headers, managed headers, hop-by-hop fields, and `Alt-Svc`. Trusted proxies (R6) are a separate configurable protection.

`New` rejects a value of `QueryPolicy` or `HeaderNamePolicy` that is not a defined constant.

## Consequences

**Positive:** Gateways that serve legacy clients can accept semicolon queries, underscore headers, and absolute-form targets with a visible configuration choice. The header protections stay in force in drop mode. The absolute form becomes RFC-compliant when it is enabled.

**Negative:** `QueryUnchecked` brings back the query confusion of T3 for middleware that parses the query. `HeaderNameDrop` loses headers that a client sends with `_`, as nginx does by default. Each relaxation moves risk to the embedder.

## Alternatives considered

- **Forward invalid header names:** It brings back the alias attack on identity headers and conflicts with the outbound check of ADR-0019.
- **Relaxation for each route:** The checks protect the routing itself, so they must run before routing.
- **A path-policy relaxation without normalize mode:** The meaning of a relaxed path policy depends on normalize mode and on path rewriting, so the two need one design.
- **Booleans for all three fields:** A mode type leaves room for more modes, such as a query mode that only accepts `;`.

## Validation / follow-up

Tests verify each relaxation and its default, that the raw query reaches the upstream unchanged, that dropped names reach no middleware, hook, or upstream, also as trailers, that the outbound check still rejects invalid names from middleware, and that an absolute-form target gives the same routing, hook inputs, and outbound request as the origin form, with the rules for scheme, user information, fragments, and the host.

**Amended 2026-11-06:** The origin form follows the same fragment rule as the absolute form: an unescaped `#` anywhere in the target is a fragment, which a request target must not have (RFC 9112, section 3.2), and the gateway rejects it with 400 and `invalid_request_target`. `url.ParseRequestURI` keeps such a `#` in the path or the raw query, where a decoded `%23` looks the same, so the core checks the raw target. A `%23` in the path is not a fragment; the path policy rejects it (`invalid_path`). This rule is part of the request-target check and cannot be relaxed. Envoy and HAProxy also reject a fragment in the request target by default (HAProxy CVE-2023-45539).
