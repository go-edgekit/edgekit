# ADR-0023: Classify failures from evidence and record transport facts

- **Status:** Accepted
- **Date:** 2026-11-04
- **Decision owners:** Project maintainers
- **Refines:** [ADR-0017](0017-failure-status-by-source.md) (the typed signals for 499, 503, 504, and 502) and [ADR-0012](0012-secure-defaults-and-configurable-protections.md) (R5 and R10)
- **Amends:** [ADR-0018](0018-completion-observes-gateway-rejections.md) (the list of core reason codes)

## Context

ADR-0017 maps a failure to a status by its source. It says that 504 for an upstream timeout, 499 for a client that is gone, 503 for no available upstream, and 502 for an invalid upstream response need a typed signal, and lists "a typed dial or request-written failure (R10), a typed selector result, or a typed failure from a hook". It also says that a timeout without such a signal is a round-trip failure (502).

This list does not fit two of the conditions:

- A client that is gone is not a dial or write fact. The evidence is the cancellation of the request context together with an error that reports the cancellation.
- With the dial and write facts as a condition, a dial timeout and a response-header timeout get different statuses, and the status depends on whether a transport reports trace events, not on the failure.

`net/http` reports these failures with types, not only with text: `context.Canceled`, `context.DeadlineExceeded`, and `net.Error` values whose `Timeout` method returns true, for HTTP/1.1 and HTTP/2. `net/http/httptrace` reports when the transport got a connection (`GotConn`) and when it wrote the complete request (`WroteRequest`). `http.Transport` can retry a request internally, so these events can come from an attempt other than the attempt that failed. `WroteRequest` can arrive from another goroutine after `RoundTrip` returned.

`ReverseProxy` reports some failures through its error handler without a source. Probes on Go 1.26 showed these cases: a client `Upgrade` value that is not printable ASCII, an upstream that does not accept the protocol switch, a 101 response body that cannot be written, a response writer that cannot hijack the connection, and a write of the 101 response after the hijack.

Two hijack errors look similar but are different. `http.ErrNotSupported` means that the response writer cannot hijack: the connection is still the gateway's, and the core can send a response. `http.ErrHijacked` means that another component hijacked the connection already. That component owns the connection, and a response from the core cannot reach the client: the server discards it and logs the write. [ADR-0022](0022-rejections-and-the-problem-response.md) says that the core never calls the renderer after a hijack.

## Decision

### Status precedence

For an error, the first rule that applies sets the status. The phase does not change.

1. **Rejection.** A processing hook that returns a rejection and no error rejects the request ([ADR-0022](0022-rejections-and-the-problem-response.md)). A rejection is not a failure, so the rules below do not apply to it.
2. **Client gone (499).** `errors.Is(err, context.Canceled)` is true, and the context that the caller gave to `ServeHTTP` (the entry context) reports `context.Canceled`. This rule applies in each processing stage: the hooks, the upstream selection, the round trip, and the protocol switch. A canceled entry context alone never changes the status of another error.
3. **Upstream timeout (504).** In the round trip only: the error is a `net.Error` whose `Timeout` method returns true. `context.DeadlineExceeded` is such an error. Dial, TLS-handshake, and response-header timeouts are included, and the status does not depend on the trace facts below.
4. **Typed sentinels.** An error that wraps `ErrNoUpstreamAvailable` from the upstream selector gives 503. An error that wraps `ErrInvalidUpstreamResponse` from an `UpstreamResponse` hook gives 502. In another phase, a sentinel has no effect: the error gets the status of its source.
5. **Source.** 500 for hooks, the selector, the outbound header check, and embedder defects (below). 502 for the round trip and for a failure that the core cannot assign to a source.

A timeout in a hook or in the selector is a failure of extension code or of its dependency, so it stays 500.

The core never reads error text. `net/http` returns the cancellation error when the request context is done, also when a dial failed for another reason at the same time. The core does not correct this.

The core keeps a private reference to the entry context in the gateway context until completion ends, only to apply rule 2. This is not a second authoritative copy in the sense of [ADR-0005](0005-per-request-gateway-context.md): completion receives the same entry context.

### Transport facts (R10)

Completion gets two facts that the transport reports through `net/http/httptrace`:

- `ConnectionObtained`: at least one attempt of the round trip reported `GotConn`.
- `RequestWritten`: at least one attempt reported `WroteRequest` without an error. This does not prove that the upstream read the request.

These are facts about the request, not about the attempt that failed. They do not show where a failure occurred, and the core does not use them to choose a status. A false value means that the fact was not established when completion started, not that the event did not occur. `http.Transport` reports both events for HTTP/1.1 and HTTP/2. Another transport establishes them only when it calls the same trace hooks.

The core gives the trace only to the request that goes to the transport. The `UpstreamResponse` hooks get the context without the trace, so a request that a hook sends cannot establish the facts.

The trace callbacks change only a separate value with atomic fields, never the gateway context, because they can run after the round trip or after completion. Completion reads that value once, when it starts, and each completion hook gets a copy of the same snapshot.

This provides two parts of R10: the original failure is kept without changes in `Failure.Err`, so typed causes such as `*net.OpError` stay available through `errors.As`, and the core records the write fact. It does not provide a typed dial failure that identifies the failed attempt. That needs a dialer that the core owns, such as the egress dialer.

### Custom transports

A transport that wants the classification of `http.Transport` must:

- return an error that wraps `context.Canceled` when it stops because the request context is canceled;
- return a `net.Error` with `Timeout() == true`, or `context.DeadlineExceeded`, for a timeout;
- call the `GotConn` and `WroteRequest` hooks of the request's `httptrace.ClientTrace` to establish the facts;
- return a 101 response with a body that implements `io.ReadWriteCloser`.

Otherwise its failures are 502, and the facts stay false.

### Protocol switch

The stage after the core accepted a 101 response, up to the end of the switch, is the phase `PhaseProtocolSwitch`. The core knows the stage from its own state, not from the error.

| Condition | Phase | Status |
| --- | --- | --- |
| The client sent `Connection: upgrade` with an `Upgrade` value that is not printable ASCII | – (secure default, reason `invalid_upgrade`) | 400 |
| Middleware passed on such a value | `BeforeUpstream` | 500 |
| The transport returned a 101 response with a body that cannot be written | `RoundTrip` | 500 |
| A response hook replaced the body of a 101 response with one that cannot be written | `UpstreamResponse` | 500 |
| The upstream does not accept the protocol switch | `ProtocolSwitch` | 502 |
| The hijack fails with `http.ErrNotSupported`: the response writer cannot hijack | `ProtocolSwitch` | 500 |
| The hijack fails with `http.ErrHijacked`: another component hijacked the connection already | `ProtocolSwitch` | none: the core never renders |
| The hijack fails with another error | `ProtocolSwitch` | rule 2, else 502 |
| A failure after a successful hijack | `ProtocolSwitch` | none: the core never renders |

For `http.ErrHijacked`, the core records the original error, without changes, as a `Failure`. It does not call the renderer, and it does not write, flush, or close the client connection, because the component that hijacked the connection owns it. No response is sent, so no status is an HTTP observation of this case. The failure is the only execution fact, and it shows a defect of the embedder: a component used the connection while the gateway handled the request.

The core checks the `Upgrade` value with the predicate of `ReverseProxy`: a `Connection` field with the token `upgrade`, and a first `Upgrade` value with a byte outside 0x20 to 0x7E. The reason code `invalid_upgrade` is added to the codes of ADR-0018.

`PhaseUnknown` stays for a failure that `ReverseProxy` reports before any stage of the core is marked.

## Consequences

**Positive:** Client disconnects and upstream timeouts no longer count as 502 in metrics. A canceled hook is not an extension defect. A connection timeout has the same status with each transport that reports timeouts by type. Embedder defects in a protocol switch are 500 with a known phase. The core never writes to a connection that it does not own, and the completion facts still show the defect.

**Negative:** Clients and dashboards see 504 and 499 where they saw 502. A custom transport that reports timeouts without a type gets 502. The trace facts cost one trace context for each proxied request. When another component hijacked the connection, the client gets no response from the gateway; what it gets depends on that component.

## Alternatives considered

- **504 only after `RequestWritten`:** The status then depends on the trace support of the transport and on the attempt that wrote first, not on the failure.
- **499 only in the round trip:** A hook that stops because the client is gone is then counted as an extension defect.
- **Per-attempt facts:** The trace events do not mark the attempt boundaries for HTTP/2 retries in `http.Transport`.
- **Status from error text, as some proxies do:** Text is not a contract, and it can contain request data.
- **Every hijack error as an embedder defect:** A writer can report an I/O failure that is not a defect.
- **500 for `http.ErrHijacked`:** The server discards the write, so the status is not sent, but response-observing middleware could record a status that the client never got.
- **No rendering only after a hijack by the core:** It keeps a write that cannot succeed.

## Validation / follow-up

Tests verify each rule with transports that control the order of the error and the cancellation, timeouts on HTTP/1.1 and HTTP/2 with `http.Transport`, a timeout without trace events, sentinels in the right phase and in another phase, the facts at completion for a closed port and for a written request, the same snapshot for two completion hooks, no facts from a request of a response hook, and each row of the protocol-switch table. For `http.ErrHijacked`, a test verifies a `Failure` in `PhaseProtocolSwitch` with the original error, with no call of the renderer and no write, flush, or header write on the response writer.
