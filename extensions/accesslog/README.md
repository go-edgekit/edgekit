# Access-log experiment

This extension tests whether the current middleware and lifecycle APIs can support useful observability without expanding the gateway core. It uses only the standard library and the existing hooks.

## Responsibilities

| Mechanism | Observations |
| --- | --- |
| Middleware | Generated request ID, middleware-derived trace ID, method, path, response status, body bytes, response-write errors, hijacking |
| Upstream-response hook | Upstream status before later hooks; removal of upstream-supplied request IDs |
| Completion hook | Final route, selected upstream origin, duration, gateway errors, and abort state |

The middleware stores a small request state under a private typed key. Completion copies its observations into a `Record` and calls the configured reporting function. No request context, response writer, or live tracing span is passed to the reporter.

## Registration

Register the same instance as both middleware and an extension:

```go
access := accesslog.New(accesslog.Config{
    Report: func(record accesslog.Record) {
        slog.Info("request completed",
            "request_id", record.RequestID,
            "status", record.StatusCode,
            "bytes", record.BytesWritten,
            "result", record.Result,
            "route_matched", record.RoutePrefix != "",
            "upstream_selected", record.Upstream != "",
            "duration", record.Outcome.Duration,
            "has_error", record.Outcome.Err != nil,
            "has_response_error", record.ResponseError != nil)
    },
})
app, err := gateway.NewWithConfig(gateway.Config{
    Routes:      routes,
    Middlewares: []gateway.Middleware{access.Middleware},
    Extensions:  []any{access},
})
```

`accesslog` is `example.com/go-gateway-starter/extensions/accesslog`. The demo in `cmd/gateway/main.go` registers it and writes records with `log/slog`.

The middleware replaces client-supplied `X-Request-ID` values with a random 128-bit ID, forwards it upstream, returns it to the client, and exposes it through `RequestIDFromContext`. Query strings, request bodies, authorization headers, and credentials in configured upstream URLs are not copied into dedicated record fields. Paths, origins, route labels, trace IDs, arbitrary method tokens, and errors may still be sensitive; the reporter owns field selection, redaction, and storage policy. Records are not safe to serialize wholesale. The demo uses log-only allowlists for method/result labels and omits raw paths, origins, route labels, trace IDs, and error text. It does not change the original record or error. See [the diagnostic audit](../../docs/investigations/sensitive-diagnostics.md) for proxy/server/process logging boundaries.

## Tracing integration

Set `Config.TraceID` to a function that extracts the current trace ID from your tracing SDK's standard context. No tracing SDK is required by this package. The demo does not install a tracer, so its `TraceID` field is empty.

Recommended registered middleware order:

```text
Tracing middleware: create a derived context and own the span lifetime
  Access-log middleware: snapshot trace ID and observe the response
    Recovery / authorization / other rejecting middleware
      Gateway lifecycle
Completion: report the snapshots after middleware unwinds
```

The tracing middleware must pass its derived context to `next`. Access-log middleware must enter before any middleware whose rejection or recovery response it should observe. Responses written outside its wrapper cannot be counted. Register the observer before other response-hook extensions if it should see responses that those extensions might reject.

Completion receives the gateway entry context, which does not contain contexts derived inside middleware. Trace correlation still works because the ID was captured while tracing middleware was active. The trace span may already have ended by completion time. This extension reports correlation; it does not end, retain, or annotate a live span.

## Interpretation and limits

- `Rejected` means a valid `gateway.HTTPError`, or a local 4xx response. A normal upstream 4xx/5xx remains `UpstreamResponse`, not a proxy execution failure.
- `ProxyError` means a non-rejection gateway error after an upstream was selected. It includes outbound-hook, transport, and response-hook failures, not just connection failures. An outbound-signing failure can have a selected upstream without any transport invocation. The current core does not expose exact error-phase provenance.
- `Upstream` names the selected origin. It does not prove that the upstream received the request or count connection attempts.
- `UpstreamStatusCode` is a snapshot at the observer's response hook. Later hooks may modify or reject it. `StatusCode` reflects the downstream response observed by middleware.
- A handler that returns normally without writing is recorded as 200. An early panic retains status 0 rather than inventing success. Panic values continue to propagate.
- Body byte counts mean bytes accepted by `ResponseWriter`, excluding headers, HTTP framing, and raw hijacked traffic. They are not a delivery guarantee. Upstream body-copy failures can produce an abort after a 200 and a partial body, without a gateway error callback.
- Protocol upgrades remain functional. The standard reverse proxy writes its upgrade handshake directly to the hijacked connection. Such records have `Hijacked: true`, observed upstream status 101, downstream status 0, and no raw-connection byte count.
- The response wrapper preserves the presence of `http.Flusher`, `http.Hijacker`, and `http.Pusher` independently. `ResponseController` flushing and hijacking remain observable; deadline and full-duplex operations pass through. Direct writes after manually unwrapping the observer bypass accounting. `io.ReaderFrom` is deliberately omitted so copies go through `Write`; deprecated `http.CloseNotifier` is not exposed. Use request cancellation instead.
- The existing synchronous-middleware contract still applies. `http.TimeoutHandler` and other middleware that return while downstream work continues are not supported. Reporting callbacks must be concurrency-safe, must not panic, and should not block for long.
- Without its registered middleware, this extension emits no record. When used outside a gateway, its middleware passes through unchanged.

## What the experiment establishes

The current completion hook is sufficient for access-log records when middleware explicitly collects HTTP accounting and correlation. Status and byte counts do not need to move into the core `Outcome`, and completion does not need to inherit an arbitrary middleware context.

This does not settle live-span annotation, asynchronous middleware, or exact proxy-error phases. Those need separate requirements rather than more generic lifecycle hooks now. See `accesslog_test.go` and `response_test.go` for executable examples, including tracing context loss, local rejection, upstream HTTP errors, proxy errors, streaming aborts, flushes, protocol upgrades, and concurrent requests.
