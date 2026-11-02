# Outbound-signing experiment

This extension tests whether route middleware can supply private typed state to a fallible hook on the actual rewritten outbound request. It uses only the public gateway API and the standard library.

It is not a production authentication protocol. The signature has no timestamp, replay protection, body integrity, key identifier, or rotation mechanism. Use TLS to protect traffic; the example does not provide it.

## Registration

```go
signer := outboundsign.New()
app, err := edgekit.NewWithConfig(edgekit.Config{
    Routes: []edgekit.Route{{
        Prefix:   "/api/",
        Upstream: target,
        Middlewares: []edgekit.Middleware{
            signer.WithKey(secret),
        },
    }},
    Extensions: []any{signer},
})
```

Here `outboundsign` is `github.com/go-edgekit/edgekit/extensions/outboundsign`, `target` is an upstream `*url.URL`, and `secret` is a non-empty `[]byte` supplied by the executable. Do not commit real secrets to source control.

Register the same instance's `WithKey` middleware on every route that can reach its globally registered hook. Missing middleware or an empty key fails closed with a generic 502; it does not silently forward an unsigned request. Different routes can use different keys with one extension instance. `WithKey` copies the supplied key at construction.

Route middleware stores the key under the extension's private typed state key. The outbound hook retrieves it only after the gateway has selected the destination and rewritten the request. No route-matched hook is needed for this experiment.

## Signing behavior

The hook replaces any client-supplied `X-Gateway-Signature` with a lowercase hexadecimal HMAC-SHA256 signature. Its input is five fields separated by a single newline, without a trailing newline:

```text
method
outbound URL scheme
outbound URL authority
outbound Host (or URL authority when Host is empty)
escaped request URI including query
```

The URI is `request.URL.RequestURI()`, not the original inbound URI. It includes the upstream base path and configured query parameters after rewriting. The hook never reads or buffers the body, so body streaming is unchanged and body contents are not authenticated.

The extension checks execution-context cancellation before signing. Missing route state, an empty key, or cancellation returns an error before the underlying transport is invoked.

## Lifecycle and errors

```text
Route matched and metadata recorded
  Route middleware installs the signing key
    Gateway selects the static upstream and rewrites the request
    BeforeUpstream hooks in registration order
      Sign the rewritten request
    Transport round trip
    Upstream response hooks and response streaming
  Route middleware unwinds
Completion hooks
```

`BeforeUpstreamHook.OnBeforeUpstream` is experimental. The gateway compiles its ordered hook list at startup and invokes it through a private `http.RoundTripper` wrapper. `ReverseProxy.Rewrite` cannot return an error, so it is not sufficient for this fallible boundary.

The wrapper clones request metadata to respect the `RoundTripper` contract. Hooks and the underlying transport receive the same clone. It does not copy the body. On a hook error, the wrapper closes the outbound body, skips later outbound hooks and transport execution, and lets the existing proxy error handler render the response. Unexpected errors produce a generic 502; typed gateway rejections retain their existing behavior. Error details remain in `Outcome.Err`.

Completion still runs once after registered middleware unwinds. A selected upstream is available even when signing fails; selection does not prove that the upstream was contacted. Existing access-log classifications may call such a failure `ProxyError`, which is an extension interpretation, not evidence of a connection attempt.

## Ordering and limits

- Register the signer after any outbound hooks that modify fields it signs. Later mutation can invalidate a signature.
- The hook runs once per proxy round trip, not once per connection attempt or standard-transport internal retry. It is not a retry-policy API.
- Hooks must preserve the execution context and must not consume or replace the body or retain the request. Destination selection remains gateway-owned.
- The underlying transport can still add protocol headers and choose wire framing. This is not access to the exact wire representation of the request.
- Extension instances are shared; request state stays private to each request and is accessed serially. Keys configured by middleware are read-only.
- The demo executable does not enable signing. The integration tests assemble a signed gateway and verify the signature at a local upstream.

## What the experiment establishes

Tests verify:

- Route middleware state and selected-route metadata are available before outbound hooks.
- The upstream receives a valid signature for its destination, method, escaped path, and merged query, including an upstream base path different from the inbound path.
- Client-supplied signatures are replaced, and inbound request metadata is unchanged.
- Request bodies arrive unchanged without signing-time buffering.
- Missing state, empty keys, and cancellation prevent forwarding while completion observes the error after middleware unwinds.
- Concurrent requests on different routes use their own signing keys.

Core lifecycle and transport tests also cover ordered outbound hooks, early-exit skipping, controlled rejections, error redaction, panic unwinding, request cloning, and body closure without transport delegation on hook failure.

This validates one missing extension boundary. It does not stabilize its Go signature, add structured failure provenance, establish production signing security, or justify route-matched hooks, replaceable selectors, retries, or asynchronous middleware.

## Assessment

The experiment supports keeping the outbound boundary as a provisional API, not treating it as finished. Its main result is useful customization through a small core change, with signing logic kept outside the core.

- **The outbound boundary is needed:** signing requires the rewritten destination and URI, which inbound middleware cannot reliably provide.
- **The standard proxy is sufficient for this use case:** a small transport wrapper provides fallible processing without forking `ReverseProxy`.
- **Middleware and hooks have complementary roles:** route middleware establishes state, the outbound hook consumes it at the required boundary, and completion observes the result after unwinding.
- **No additional lifecycle phase was needed:** route-matched hooks should remain deferred until another concrete use case requires them.

This assessment does not change ADR statuses or freeze the public API.

## Open questions before stabilization

### Failure classification and HTTP responses

Unexpected signing errors currently inherit the proxy error handler's generic 502. A gateway signing or configuration failure is not necessarily an upstream failure. Before stabilizing error semantics, decide whether these cases need different HTTP responses and structured failure provenance. Do not infer a connection attempt from a selected upstream or the access-log extension's `ProxyError` classification.

### Mutable request access

The hook receives a mutable `*http.Request`, while its contract limits customization to headers and reserves destination, context, and body ownership. Those restrictions are behavioral contracts for trusted compiled-in extensions, not enforced isolation. Keep this limitation explicit; do not add a restrictive request facade without another use case to guide its design.

### Registration and configuration validation

The globally registered signer requires matching route middleware on every forwarded route. Missing state correctly fails closed, but configuration mistakes are discovered during requests. This is acceptable for the experiment, not necessarily for a production extension. Revisit validation and registration ergonomics when a real integration needs them.

### Scope of the evidence

The experiment validates header-oriented signing, not body transformation, per-attempt signing, retry handling, or a complete authentication protocol. Do not generalize those capabilities from the current tests.

## Recommended next steps

1. Keep the outbound hook provisional and preserve its regression tests. Do not add more lifecycle hooks or turn the signing example into a production feature now.
2. Move to the focused proxy-hardening work described in [ADR-0010](../../docs/adr/0010-scaffold-hardening-and-verification.md).
3. Revisit API stabilization after another independently useful extension exercises the boundary, using that evidence to resolve the questions above.
