# ADR-0024: Hardened server: explicit relaxation, deadline ownership, and upgraded connections

- **Status:** Accepted
- **Date:** 2026-11-05
- **Decision owners:** Project maintainers
- **Refines:** [ADR-0012](0012-secure-defaults-and-configurable-protections.md), [ADR-0016](0016-core-owns-the-inbound-server.md)

## Context

[ADR-0016](0016-core-owns-the-inbound-server.md) gives the core the inbound server, and [ADR-0012](0012-secure-defaults-and-configurable-protections.md) makes header budgets and HTTP/2 limits (R8) and progress-based idle deadlines (R9) secure defaults. ADR-0012 also says that streaming routes may relax the idle deadlines, and that relaxation is never the result of an omitted value.

These decisions leave open how a caller disables a timeout, who owns a deadline when a handler sets one, how a deadline measures progress, and what happens to a connection after a protocol switch. `http.Server` does not apply its timeouts to a hijacked connection and does not close it at shutdown. `http.ResponseController` deadlines are absolute: a deadline set before one large write limits the total time of the write, not its progress.

## Decision

**Configuration.** In the server configuration, zero means the secure default and a positive value replaces it. The named sentinel `NoTimeout` disables a timeout. All other negative durations, and negative or out-of-range sizes, are configuration errors. Size and stream limits have no "unlimited" value.

**Transfer idle deadlines.** The server sets a deadline before each read of the request body, each part of a response write, and each flush, and removes it when the call returns. It splits writes into parts of at most 16 KiB, so the deadline measures progress, not the total time. This sets a minimum rate of 16 KiB for each timeout. It sets deadlines also for the reads that `net/http` does itself: when it closes the request body, before it writes the response headers, and after the handler returns. When `net/http` can read an unread body before a write (HTTP/1.1 without full duplex), the write gets one more timeout. A write never changes the deadline of a body read in progress, so response progress cannot hide a stalled upload. On HTTP/2, the deadlines apply to one stream.

**Deadline ownership.** A successful `SetReadDeadline` or `SetWriteDeadline` call of `http.ResponseController` gives that direction to the caller for the rest of the request. The server then does not set or remove deadlines for that direction, also not when the handler returns. A call that fails keeps the protection. This is how a route relaxes the deadline: route middleware sets its own deadline. No route field is necessary.

**HTTP/2 connection protection.** The transfer idle timeout is also the HTTP/2 connection write timeout (`WriteByteTimeout`). It is a protection of the connection, independent of the deadlines of each stream. A takeover in one request does not change it. Only the server configuration does.

**Upgraded connections.** The server manages only the connections that the gateway itself takes to forward a 101 response. A connection that middleware or other code takes with `Hijack` belongs to that code. On a managed connection, a read times out when no data moved in either direction for the timeout, and a write sends at most 16 KiB for each deadline. The server applies these deadlines below TLS and never retries a write that timed out.

**Shutdown.** `Shutdown` closes the managed upgraded connections before it waits for the other requests, and closes a protocol switch that completes during the shutdown. It waits for all handlers, also those of upgraded connections, so completion ran when it returns. When its context ends first, it returns the context error, as `http.Server.Shutdown` does, and the caller uses `Close` to stop the remaining requests.

**Writer capabilities.** The wrapped response writer has the same optional interfaces as the writer of `net/http`, such as `http.Hijacker` only for HTTP/1.1. It has its own deadline methods, so `http.ResponseController` cannot bypass the ownership rule. Its `Unwrap` method gives the writer of `net/http`, so `http.ResponseController` can reach methods that the wrapper does not have. Code that calls `Unwrap` itself and sets a deadline on that writer bypasses the rule, and the server can remove that deadline.

## Consequences

**Positive:** An omitted value never disables a protection, and a disabled timeout is visible in review. Healthy streams, quiet streams, and slow but active transfers continue. A stalled HTTP/2 stream does not stop its neighbors. Upgraded connections get the same idle protection as requests and do not delay or survive shutdown.

**Negative:** Each write and read costs extra deadline calls. A handler that sets a deadline for its own reason also removes the idle protection for that direction. An upgraded connection is closed at shutdown without a grace period, so clients must reconnect. A transfer slower than 16 KiB for each timeout stops.

## Alternatives considered

- **Negative duration disables:** Simpler, but an accidental negative calculation disables a protection.
- **Handler deadlines only as a cap:** Safer against accidental relaxation, but a streaming route could not relax the deadline without a new route field.
- **A grace period for upgraded connections at shutdown:** Upgraded connections do not end by themselves, so a grace period mostly delays shutdown.
- **Close all connections when the shutdown context ends:** Simpler for callers, but different from `http.Server.Shutdown`.
- **Retry a write after a timeout with partial progress:** Not safe through TLS, which keeps a write error.
