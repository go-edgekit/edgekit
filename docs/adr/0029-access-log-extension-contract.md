# ADR-0029: Define the contract of the access-log extension

- **Status:** Accepted (registration superseded in part by [ADR-0030](0030-typed-hook-phase-lists.md))
- **Date:** 2026-11-06
- **Decision owners:** Project maintainers
- **Refines:** [ADR-0009](0009-outcomes-errors-and-early-responses.md), [ADR-0014](0014-multi-module-repository.md), and [ADR-0018](0018-completion-observes-gateway-rejections.md)

## Context

ADR-0009 gives HTTP observations to response-observing middleware and execution facts to completion. ADR-0014 says that each bundled extension defines the narrow sink interface that it needs. The access-log extension joins both kinds of information in one record and gives the record to a sink.

The record contains values that must not reach a log without a policy: the request path, the method token, a trace ID from a tracing SDK, errors, and the URL of the selected upstream, which can contain user information from the configuration. A record that is easy to log completely is a leak by default.

ADR-0018 says that completion observes the rejections of the secure defaults so that access logs see them. These rejections occur before any middleware, so the middleware of the extension never sees them.

Completion hooks must not panic, and the core does not recover a panic of a completion hook: the later completion hooks then do not run. A sink is code of the embedder.

## Decision

### Package and registration

The extension is the package `github.com/go-edgekit/edgekit/extensions/accesslog` in the core module, because it uses only the standard library (ADR-0014). It uses only the public contract (ADR-0011). A lint rule forbids imports of internal packages in bundled extensions and examples.

One `Recorder` value is the middleware and the hook provider. It is the first global middleware and the first hook provider, so it observes the responses of all other middleware and runs last in the response phases (ADR-0021).

### Sink

The extension defines `Sink` with one method, `Report(Record)`, and the adapter `SinkFunc`. The core defines no sink abstraction.

- The recorder calls `Report` once for each request, at completion, synchronously, on the goroutine of the request. A slow sink delays the end of the request; the admission budgets do not limit this work, because the core releases their slots before completion. A sink must not block. Buffering, dropping, and export are the responsibility of the sink and of separate modules.
- The recorder recovers a panic of `Report` and drops that record, so the other completion hooks still run and an original panic of the request continues. The recorder cannot report the drop, because extensions do not get the internal diagnostic logger.
- A record is a value that the sink can keep.

### Record

A `Record` has these parts:

- HTTP observations of the middleware: the final status, the body bytes that the writer accepted, the first write or flush error, and a hijack. A panic before a write gives status 0, never an invented status.
- The upstream status that the `UpstreamResponse` hook of the recorder observed.
- The method, the escaped path, the request ID, and the trace ID, as snapshots of the middleware.
- The `Completion` of the core, without changes.

Each field is safe by default or sensitive. The safe fields contain no request content, no credentials, no query, no body, and no error text: the observed flag, the request ID, the status, the byte count, the hijack, the upstream status, and the start time, the duration, the rejection reason, the failure phase, the transport facts, and the abort flag of `Completion`. The other fields are sensitive: the method, the path, the trace ID, the write error, `Failure.Err`, `SelectedUpstream`, and `RoutePrefix`.

`Record` implements `slog.LogValuer` and gives only the safe fields, with booleans for the presence of a route, an upstream, and a write error. So a record that is logged completely leaks no sensitive field. A sink that needs a sensitive field selects it and applies its own policy.

The extension has no result classification. The rejection reason and the failure phase keep the distinctions that such a classification would flatten.

### Record without middleware

When the middleware did not handle a request, such as for a rejection by the secure defaults or when the middleware is not registered, the recorder still reports a record. `Observed` is false, and only `Completion` is set. The status of a secure-default rejection is not observable, because completion gets only the reason (ADR-0018); the record does not infer it.

Outside a gateway, the middleware calls the next handler without changes and records nothing, because no completion runs.

### Request ID

The middleware makes a random 128-bit request ID for each request, as 32 lowercase hexadecimal digits. The recorder declares `X-Request-ID` as a managed header, so the core removes the values of the client. It sets the ID in the response headers, in the outbound request through its `BeforeUpstream` hook, and removes the values of the upstream from the response, so the client gets only the generated ID. A client value is never trusted; accepting one needs trusted-proxy mode (R6).

### Trace ID

A function in the configuration reads the trace ID from the request context in the middleware, while the tracing middleware outside the recorder is active (ADR-0005). The extension has no tracing dependency and does not check the value.

## Consequences

**Positive:** Access logs see every request, also the rejections by the secure defaults. The easy way to log a record is the safe way. A defective sink cannot stop other completion hooks. The HTTP observations and execution facts of ADR-0009 are sufficient for an access log through the public contract.

**Negative:** A record without middleware observations has no status. A dropped record is silent. A slow sink delays responses and shutdown. The upstream status is absent when an inner response hook rejects or fails, because the hook of the recorder then does not run.

## Alternatives considered

- **A function field instead of an interface:** Simple, but the roadmap and ADR-0014 name a sink interface, and an interface lets adapters in other modules carry state. `SinkFunc` keeps the function form.
- **No record without middleware:** Simpler, because each record has HTTP observations, but it hides the rejections that ADR-0018 makes visible.
- **Let a sink panic propagate:** It obeys no contract: a completion hook must not panic, and the panic would skip the other completion hooks.
- **Strip the sensitive fields from the record:** It makes the record safe, but a sink with its own policy could not use the path or the errors.
- **A separate request-ID extension:** A cleaner split, but the access log is the only consumer of the request ID.

## Validation / follow-up

Tests verify the join of observations, facts, and trace IDs on HTTP/1.1 and HTTP/2; each gateway response and failure, including routing misses, body-limit and admission rejections, and `PhaseResponseBody`; panics; partial writes; flushes before completion; protocol switches with the management of the hardened server; concurrent records; the record without middleware; the pass-through outside a gateway; a sink panic; the fields of `LogValue`; and the optional interfaces of the response writer.

The ownership of the request ID covers all parts of the response that the client gets. The `UpstreamResponse` hook removes the upstream values from the response headers and from the declared trailers. The response writer removes the ID from informational (1xx) responses other than 101 and sets it again for the final response. When the next handler returns, the middleware removes the trailer entries of the ID that `ReverseProxy` wrote with `http.TrailerPrefix`. Tests verify the 1xx response, a declared trailer, and an undeclared trailer on HTTP/1.1 and HTTP/2.

**Registration superseded in part 2026-11-07:** [ADR-0030](0030-typed-hook-phase-lists.md) registers the recorder first in `BeforeUpstream`, last in `UpstreamResponse` and `RequestCompleted`, and separately in `HeaderOwners`. Middleware nesting and the observation contract do not change.
