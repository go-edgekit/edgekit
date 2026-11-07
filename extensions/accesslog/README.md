# accesslog

A bundled extension that makes one access-log record for each request. It uses only the public API of edgekit and the standard library. The contract is in [ADR-0029](../../docs/adr/0029-access-log-extension-contract.md); the GoDoc gives the details.

## Registration

Register one `Recorder` as global middleware, in its three hook phases, and as a header owner. Put tracing middleware outside it. In `BeforeUpstream`, put the recorder before hooks that read, change, or sign `X-Request-ID`; other hooks can get the ID with `Recorder.RequestID` at any position. Put the recorder last in `UpstreamResponse` and `RequestCompleted`. Each list runs in its written order ([ADR-0030](../../docs/adr/0030-typed-hook-phase-lists.md)):

```go
recorder, err := accesslog.New(accesslog.Config{
    Sink:    sink,    // required
    TraceID: traceID, // optional: reads the trace ID from the request context
})
if err != nil {
    return err
}

gateway, err := edgekit.New(edgekit.Config{
    Middleware: []edgekit.Middleware{tracing, recorder.Middleware},
    Hooks: edgekit.Hooks{
        BeforeUpstream: []edgekit.BeforeUpstreamHook{
            recorder,
            signer,
        },
        UpstreamResponse: []edgekit.UpstreamResponseHook{
            recorder,
        },
        RequestCompleted: []edgekit.RequestCompletedHook{
            recorder,
        },
    },
    HeaderOwners: []edgekit.HeaderOwner{
        recorder,
        signer,
    },
    Routes: routes,
})
```

```text
Tracing middleware: makes the span context
  Recorder middleware: request ID, trace ID snapshot, response observations
    Other middleware, routing, hooks, proxy
Completion: the recorder reports after the middleware returned
```

The recorder cannot detect a missing registration. Without `RequestCompleted`, the sink gets no records. Without the middleware, records have `Observed` false. Without `BeforeUpstream`, the upstream gets no request ID. Without `UpstreamResponse`, `UpstreamStatus` is always 0. Without the header-owner registration, middleware and hooks that run before the recorder can see the `X-Request-ID` values of the client. The package documentation lists each effect.

## Records

| Source | Fields |
| --- | --- |
| Middleware | `RequestID`, `Method`, `Path`, `TraceID`, `Status`, `BytesWritten`, `WriteErr`, `Hijacked` |
| `UpstreamResponse` hook | `UpstreamStatus` |
| Core completion | `Completion`: route, selected upstream, rejection reason, failure with phase, transport facts, abort, timing |

Safe by default: `Observed`, `RequestID`, `Status`, `BytesWritten`, `Hijacked`, `UpstreamStatus`, and the start time, duration, rejection reason, failure phase, transport facts, and abort flag of `Completion`.

Sensitive: `Method`, `Path`, `TraceID`, `WriteErr`, `Completion.Failure.Err`, `Completion.SelectedUpstream` (it can contain user information and a query), and `Completion.RoutePrefix`.

`Record` implements `slog.LogValuer`: `slog.Info("request", slog.Any("record", r))` writes only the safe fields. A sink that needs a sensitive field must select it and apply its own policy. The [example program](../../examples/gateway) writes only numbers, booleans, and values from allowlists.

When the middleware did not handle a request, for example a rejection by the secure defaults, the record has `Observed == false` and only `Completion`. It has no status, because completion does not get the status of such a rejection. Outside a gateway, the middleware calls the next handler without changes and records nothing.

## Sink rules

- `New` returns `ErrNoSink` for a nil sink, also a typed nil value such as `SinkFunc(nil)`, a nil pointer, map, slice, or channel, with the same rule as the extension points of the gateway. This is a startup error, not a silent loss of records.
- `Report` runs synchronously at completion, on the goroutine of the request. A slow sink delays the end of the request and `Server.Shutdown`. Admission budgets do not limit it, because their slots are free before completion. Do not block: send the record to a bounded queue, and drop it when the queue is full.
- `Report` must be safe for concurrent use and must not panic. The recorder recovers a panic and drops the record, so the other completion hooks still run. The drop is silent.

## Request ID

The middleware makes a random 128-bit ID (32 lowercase hexadecimal digits). `X-Request-ID` is a managed header, so the client cannot set it. The recorder sends the ID to the upstream and in the final response headers to the client, and gives it to hooks through `Recorder.RequestID`. The client gets no other value: the recorder removes the values of the upstream from the final response headers, from informational (1xx) responses, and from declared and undeclared response trailers. A 1xx response carries no request ID.

## Response writer

The writer of the middleware keeps the optional interfaces of the original writer (`http.Flusher`, `http.Hijacker`, `http.Pusher`) and has `Unwrap`, so `http.ResponseController` reaches deadlines and full duplex. A hijack through the controller is still recorded. Writes and flushes go through at once. The writer has no `io.ReaderFrom`, so copies go through `Write`. Writes to the result of `Unwrap` are not recorded.

## Limits

- Byte counts are bytes that the writer accepted, not bytes that the client got.
- A protocol switch is recorded as a hijack, with upstream status 101, no status, and no bytes of the upgraded connection.
- `UpstreamStatus` is 0 when a response hook that runs before the hook of the recorder rejects or fails.
- Responses written outside the middleware, such as by middleware outside it or by the secure defaults, have no status in the record.
