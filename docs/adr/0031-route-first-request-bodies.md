# ADR-0031: Match routes before request-body processing

- **Status:** Accepted
- **Date:** 2026-11-07
- **Decision owners:** Project maintainers
- **Supersedes in part:** ADR-0003, ADR-0004, ADR-0018, and ADR-0025 (handling order, request hook, provisional body limits, and admission coverage); ADR-0027, ADR-0029, and ADR-0030 (references to global middleware and its observation coverage); ADR-0019 (path/query rewrites only through route configuration, and checks of available trailer values)

## Context

A provisional gateway body limit can stop a shared body reader before the route is known. A larger route limit cannot repair that failure. The gateway default must be an inherited value, not a deployment ceiling. Route matching must precede all permitted body reads.

ADR-0025 discusses body reads in RequestReceived hooks, but the public hook contract forbids them. This decision retains the restriction and replaces the phase with a route-aware phase. Accepted rationale in earlier ADRs remains historical.

## Contract

The gateway initializes request context and completion, validates the inbound request, removes identity and managed headers, acquires gateway admission, and matches one route. It installs that route's effective body limit, checks the declared length, acquires route admission, runs RouteMatched hooks, optionally buffers the request, runs shared middleware and then route middleware, selects the upstream, prepares and validates the outbound request, and forwards it. Response hooks, informational responses, trailers, upgrades, cancellation, and shutdown retain their contracts. Cleanup and admission release run on return and panic, before completion.

`Config.SharedMiddleware` replaces `Config.Middleware`. There is no compatibility field or unrestricted outer middleware. Middleware nests in written order. `RouteMatchedHook`, `Hooks.RouteMatched`, and `PhaseRouteMatched` replace the RequestReceived names. These hooks cannot change the request or read its body. They can set private request state, reject, or fail. They run only after a successful match, declared-length check, and admission. A match alone does not establish that a hook ran. They receive the entry execution context, not a context derived by later middleware.

Completion observes route misses and early rejections without ordinary processing middleware. Access-log records for these requests have no middleware observations or generated request ID. Completion does not invent a status from a reason.

### Fixed matching and outbound rewrites

Matching uses the validated initial escaped path, with the longest-prefix rules of ADR-0020. The original routing input and matched route are immutable execution facts. Later middleware cannot change the route, selector, admission, or effective limit. There is no rematching and no body-dependent routing.

Middleware can pass a copied request with changed headers, method, Host, path/query, body/trailers, and a derived context. Path/query changes are explicit outbound rewrites, not routing inputs. The core validates the outbound representation against the applicable request-target, path, query, header, and protocol checks. Invalid middleware output is an internal failure in PhaseBeforeUpstream, not a client rejection. Only the matched route's selector chooses the destination; a middleware URL cannot supply another scheme or authority. Middleware owns the bounds and closure of a replacement or expanded body. The inbound limit does not bound independently generated content.

### One inbound limit

The omitted, finite (including zero), and explicitly disabled BodyLimit values remain distinct. Resolve the effective value once. The gateway default remains 10 MiB, not a hard ceiling.

The limit counts bytes exposed by the inbound HTTP body reader, without transfer framing or optional decompression. It does not bound all network bytes, server cleanup reads, decompressed output, or generated bodies. Streaming remains the default, without complete-body buffering or background read-ahead. Actual reads enforce the limit. Unknown-length overflow can follow partial upstream forwarding. The core never renders a new response after final commitment. Typed errors, request-specific overflow evidence, closure, cancellation, and trailer checks remain in force.

### Explicit memory buffering

`Route.RequestBuffering *RequestBuffering` enables buffering. Nil means streaming. RequestBuffering contains `Budget *RequestBufferBudget`, `IdleTimeout`, and `TotalTimeout`. `NewRequestBufferBudget(bytes)` requires a positive byte count. Budgets can be shared across routes and gateways. Configuration requires a finite effective body limit, a budget large enough for that limit, and positive explicit durations. There are no buffering timeout defaults or disabled buffering timeouts.

A request without a body (a nil Body or http.NoBody) skips buffering. It reserves nothing, needs no deadline support, and gets a GetBody that returns http.NoBody, which holds no storage. A request with a declared length reserves that length; the declared-length check has already rejected a length over the limit. A request with an unknown length reserves the complete effective limit. The gateway reserves before reading, without a queue. Reservation exhaustion gives 503 with ReasonRequestBufferSaturated. A route with a 100 MiB limit reserves 100 MiB for a small body of unknown length. This is reserved capacity, not actual allocation. The effective body limit remains the acceptance limit, not a second storage maximum.

A declared Content-Length of zero with a body that is not http.NoBody still buffers to a checked EOF, because HTTP/2 trailer fields can follow an empty body. A source that gives data after its reservation fails the buffering as an internal failure; net/http ends a body at its declared length, so such data is not client evidence.

Storage is memory-only. Buffering allocates one bounded segment at a time, before the reads that fill it, and never more segment capacity than the reservation. There is no disk spill, temporary file, body-sized copy per replay, or response buffering. The shared budget bounds reserved stored-body capacity, not process memory: metadata, scratch space, runtime allocation overhead, copies that consumers make when they read replays, and other request work are outside it. Dropping storage references does not require immediate garbage collection or memory return to the operating system. A fatal Go allocation failure is not recoverable as an HTTP storage failure. Deployment memory headroom remains necessary.

Buffering must reach a checked successful EOF within the reservation before processing middleware, upstream selection, or normal forwarding. Checked client trailers survive replay. Before forwarding, middleware trailer values take precedence over unchanged client values in every letter case; deleted names stay deleted. The outbound check validates trailer values that are already available, as well as their names. Invalid values from processing are internal failures, not round-trip failures. Oversized or failed buffered bodies never reach the normal upstream path. Indefinite request streams must use streaming routes.

Gateway and route admission, when configured, cover buffering and its storage lifetime. Ordinary header-only rejection middleware runs after buffering; restricted RouteMatched hooks are the place for checks that must run before it. This is a deliberate resource trade-off, not an additional middleware system.

### Replay and resource lifetime

Buffered requests expose Body and GetBody to processing middleware. GetBody creates an independent cursor over shared immutable chunks. Concurrent independent readers and repeated replay creation are supported; access to one cursor is also serialized. Replays do not consume the inbound limit again. Reading Body consumes that cursor: middleware must explicitly get a replay and replace Body if it wants to forward the complete content. Replacement retains the middleware closure and trailer responsibilities.

The gateway clears a caller-provided inbound GetBody on its request copy, so it cannot expose an unprotected source-body replay to processing extensions. Streaming supplies no gateway-owned replay. Middleware-generated replacement representations remain the middleware's responsibility.

When the outbound request still has an unread gateway cursor, an unknown length, no trailer fields, and stored content that is not empty, the gateway sets its ContentLength to the stored size and removes the inbound chunked transfer coding. The upstream then gets a declared length. Transfer coding is a property of one connection (RFC 9112, section 6.1), so this does not change the content. The gateway never changes an unknown length to zero: ReverseProxy then sends no body, and net/http can retry a request without a body. Trailer fields keep chunked framing, because HTTP/1.1 sends them only in the chunked coding. A consumed, partly read, or wrapped cursor keeps its length unknown, because middleware owns that representation.

The outbound transport never receives the new replay capability as Request.GetBody. The core clears GetBody at the transport boundary. Go's HTTP/1.1 transport uses it in rewindBody and shouldRetryRequest; HTTP/2 uses it in http2shouldRetryRequest. Existing transport retries without body replay remain delegated to net/http. This change adds no retry path and does not make arbitrary requests safe to retry.

The reservation lasts through the entire protected route handling, including a stalled or streaming response or an upgraded connection, even if the transport closes its upload cursor early. All gateway-owned cursors and replay functions are request-scoped. Consumers must not retain them after handling. Closing one cursor releases its reference without invalidating others. On every exit, including failure, cancellation, and panic, cleanup locks the shared store, invalidates all remaining cursors and replay functions, drops the chunks, then releases the reservation. Retained invalid handles hold no stored-body data and fail with io.ErrClosedPipe. No usable gateway-owned handle can refer to storage after reservation release.

### Deadline ownership

Buffering a request with a body requires the scoped deadline support of Server; an ordinary streaming gateway and a request without a body do not. A writer without this support fails with 500 before any buffering read, and Completion.Failure holds the cause. New cannot check the support, because it does not know which server will call the gateway. A wrapper can preserve support through Unwrap. Setting a public read deadline alone cannot prove support, because net/http has no operation to read an existing deadline or restore its ownership.

The server installs a private temporary scope in its existing transfer deadline manager. Before each blocking read, it sets the earliest of the total deadline, buffering idle deadline, normal server idle deadline, and any recorded caller-owned deadline. Zero deadlines do not limit the choice. It never extends a stricter applicable deadline. It checks deadline-setting failures before reading. Total-duration checks also apply before and after reads satisfied from already buffered data, and before acceptance. Cancellation interrupts a blocking read with a deadline, not a detached reader goroutine. The cancellation callback records whether it shortened a still-future deadline. This prevents a context cancellation caused by net/http's read timeout from changing an established 408 to 499.

The first HTTP/1.1 body read can write 100 Continue. The scope also bounds that write by the buffering deadlines and any stricter recorded write deadline, without a write-ownership takeover. It restores the prior write deadline and owner at scope exit. Failed deadline calls establish no timeout or cancellation evidence.

The temporary scope does not change read ownership. Cleanup stops and joins cancellation callbacks, removes the scope, and restores the recorded caller deadline if that direction was caller-owned. Otherwise it restores normal server deadline management. Clearing a deadline alone is not ownership restoration. The same restoration runs after success, read failure, cancellation, and panic. A failed restoration fails closed; an expired read deadline can make the connection unusable, as net/http documents.

Code that bypasses the manager through Unwrap and changes the underlying writer's deadlines is outside the ownership contract, as in ADR-0024.

### Failure evidence

Declared oversize remains a 413 rejection with ReasonBodyTooLarge. Buffer reservation exhaustion is a 503 rejection with ReasonRequestBufferSaturated. Actual read failures during buffering use PhaseRequestBuffering and retain their typed causes. Existing precedence remains: established client cancellation first, then this request's overflow evidence (413), then this request's invalid-trailer evidence (400).

400 also applies to established malformed or truncated inbound bodies (io.ErrUnexpectedEOF from the inbound read, or a declared-length mismatch at checked EOF), not arbitrary reader errors. 408 requires an inbound deadline error with evidence from the active read deadline, not merely a net.Error implementation. Unknown reader errors, deadline-support failures, and internal defects are 500. Go's HTTP/1.1 chunk decoder and HTTP/2 declared-length checks return some input failures as plain errors, without typed parser evidence. Those failures remain 500; the core does not inspect their text. A timeout or cancellation in another phase retains that phase's existing classification. Responses contain no body data, error text, allocation details, or storage settings. Diagnostics use internal/safelog and safe classifications only.

## Validation

Tests must cover route-limit inheritance and overrides before shared readers; stable routes and valid/invalid rewrites; hook, middleware, and completion ordering; declared and actual lengths on HTTP/1.1 and HTTP/2; streaming and buffered trailers; streaming commitment and partial upstream data; admission coverage; cancellation, panic, shutdown, and upgrades; malformed and unknown read failures; budget exhaustion and cleanup; requests without a body, declared-length reservations, data after a reservation, the forwarded length of buffered content, and empty bodies of unknown length that stay non-retryable; shared replay storage, concurrency, invalidation, and slow-response lifetime; stricter deadlines and ownership restoration; and absence of new transport replay retries. Resource tests establish the stated storage bounds without a process-memory or performance claim.
