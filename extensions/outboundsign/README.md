# outboundsign

**Status:** demonstration. This is not an authentication protocol, and the project does not support it as a security feature.

The extension signs the outbound request of a route with HMAC-SHA256 (RFC 2104). It shows that a `BeforeUpstream` hook gets the request after upstream selection and rewriting, and that route middleware can give the hook its route-specific settings. It uses only the public API of edgekit and the standard library.

## Limits

The signature has no timestamp, no nonce, no replay protection, no body integrity, and no key identifier, and there is no key rotation. A party that sees one signed request can send it again, with any body, while the key is valid. Use TLS to the upstream, and do not let the upstream rely on the signature alone. The [threat model](../../docs/threat-model.md) lists these limits.

## Registration

```go
signer := outboundsign.New()
withKey, err := signer.WithKey(key) // copies key; an empty key is an error
if err != nil {
    return err
}

gateway, err := edgekit.New(edgekit.Config{
    Hooks: edgekit.Hooks{
        BeforeUpstream: []edgekit.BeforeUpstreamHook{
            signer,
        },
    },
    HeaderOwners: []edgekit.HeaderOwner{
        signer,
    },
    Routes: []edgekit.Route{
        {
            Prefix:     "/api/",
            Upstream:   selector,
            Middleware: []edgekit.Middleware{withKey},
        },
    },
})
```

Put the signer last in `BeforeUpstream` so it signs the header changes of earlier hooks. Register it in `HeaderOwners` so the core removes client values of its managed header. The route middleware gives the key to the hook through request state. One `Signer` can serve many routes with different keys. This is one way to give a hook route-specific settings; it does not decide between route bindings and route middleware ([roadmap](../../docs/roadmap.md), section 3).

## Signature

`X-Gateway-Signature` is the lowercase hexadecimal HMAC-SHA256 of five fields, separated by a newline, with no newline at the end:

```text
method
outbound URL scheme
outbound URL authority
Host value that the transport sends
escaped request URI with the query
```

The fields come from the outbound request after the rewrite, so the base path of the upstream is included. The hook does not read, buffer, or change the body or the trailers. `X-Gateway-Signature` is a managed header, so the gateway removes the values of the client.

## Failures

The hook fails closed. The transport does not run, and completion runs after the middleware returned:

| Condition | Error | Status |
| --- | --- | --- |
| The route has no `WithKey` middleware | `ErrNoKey` | 500 (an extension failure, ADR-0017) |
| A signed field other than the request URI contains a newline, such as a method that is not a token | `ErrInvalidField` | 500 |
| The client is gone | the error of the context (`context.Canceled`) | 499 (ADR-0023) |
| A middleware context is done | the error of the context | 500 |

The errors contain no key data. `WithKey` returns `ErrShortKey` for a key shorter than `MinKeySize` (32 bytes, the output size of SHA-256, RFC 2104, section 3), before the gateway serves.
