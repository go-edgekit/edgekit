# Resource limits and slow-body boundaries

**Status:** Initial implementation of pass 3, not an architecture decision or production-readiness claim

This implements a bounded slice of [the follow-up plan](proxy-risk-follow-ups.md#3-resource-limits-and-slow-bodies): explicit example header budgets and optional fail-fast admission. Workload-specific slow-body enforcement remains deployment-owned. No accepted ADR rationale, lifecycle hooks, core outcomes, signing API, or framework defaults change.

## Ownership and established protections

| Owner | Boundary |
| --- | --- |
| Framework | Streaming, request cancellation, existing proxy error rendering, and outer completion remain unchanged. No mandatory concurrency or size policy. |
| Example executable | `http.Server.MaxHeaderBytes = 64 KiB`; reusable transport `MaxResponseHeaderBytes = 64 KiB`; one global admission budget of 64 downstream calls with zero admission waiters, after access-log middleware. |
| Optional middleware | [`extensions/admission`](../../extensions/admission/README.md) supplies a shared synchronous budget; `extensions/bodylimit` still supplies the 8 MiB example body-read limit. |
| Serving edge / deployment | Connection limits, per-stream slow-upload and slow-reader policies, admission capacity selection, and separate budgets for long-lived traffic. |

The header settings use Go's native parser/protocol semantics, not an exact universal wire-byte ceiling. Inbound HTTP/1 parsing has a look-ahead allowance; HTTP/2 header lists use decoded protocol accounting. Review the supported Go version and protocol before choosing a budget. The example serves plaintext HTTP/1; upstream tests also exercise standard HTTP/2. The egress transport already copies the template's upstream header limit when enabled explicitly.

Inbound header rejection happens in `net/http` before the gateway handler runs. Such a 431 does not initialize gateway context or run completion/access-log hooks. Do not invent a gateway outcome for pre-handler work. Oversized upstream headers fail transport parsing before response hooks/final downstream headers; the existing handler returns a generic 502 and completion retains the transport error. Normal-sized headers and streamed bodies remain unaffected.

All existing connection/header/idle timeouts remain in place. No blanket `ReadTimeout`, `WriteTimeout`, request-body deadline, total stream deadline, or asynchronous handling was added. The example limits remain source-level choices, not new environment configuration.

## Admission and lifecycle

The admission budget counts calls to downstream handlers until they return or unwind, not connections or confirmed client delivery. Each request, including each HTTP/2 stream, must acquire a slot without waiting. Saturation produces a generic local 503. It neither calls later middleware/hooks nor reads a body. One middleware instance shares a budget across its wrapped handlers; separate constructors have independent budgets. This is not rate limiting, authentication, or a fairness guarantee.

Admitted requests hold slots during uploads, response streaming, and transport-pool waits. Cancellation does not release a slot while downstream still runs. Defers release slots on normal handling and panics without swallowing `http.ErrAbortHandler`. Completion remains outside the chain and runs after admitted or rejected handling unwinds. A local 503 is an HTTP observation, not a new `Outcome.Err` or proof of an upstream connection attempt.

There is no admission queue. Native pool acquisition may still wait, bounded in request count by the admission budget, with cancellation supplied by request contexts. The budget does not cover outer middleware, completion callbacks, pre-handler work, overflow-response writes, or server body-draining/cleanup after handler return. It cannot cap every goroutine or socket in the process. Custom downstream code that ignores cancellation can continue occupying a slot; work continued after handler return remains outside the supported lifecycle.

## Slow-body policy still required

Choose these policies for each deployment before adding enforcement. A concurrency cap contains admitted work but does not stop a client from occupying capacity forever.

1. **Uploads:** define acceptable pauses, minimum progress, maximum encoded/decompressed sizes, and legitimate large-upload durations. Enforce appropriate header/body progress and connection policies at the serving edge. Rejecting a request without reading its body does not guarantee immediate socket cleanup; `net/http` may drain unread bytes after handler return.
2. **Slow readers:** choose write-idle or blocked-write policies separately from response duration. A healthy SSE stream can have long quiet intervals; those are not automatically a slow-reader failure. Preserve backpressure rather than buffering bodies to free slots.
3. **HTTP/2:** use per-stream policies where required. A connection-wide deadline can terminate unrelated multiplexed streams. Bound accepted connections and protocol stream concurrency at the serving edge in addition to application admission.
4. **Workload isolation:** decide whether uploads, long-lived streams, and ordinary RPCs need separate budgets. One global example budget can be exhausted by 64 healthy streams. Route-scoped independent middleware is available, but deployment capacity and reservation policy are not selected by the framework.
5. **Outer work and reporting:** bound logger/exporter/callback behavior and rejected-response work independently. Keep completion synchronous and do not release permits using detached timers while downstream continues.

There is intentionally no generic idle-body wrapper in this pass. Implement one only after defining a concrete workload, protocol, deadline ownership/reset rules, behavior on unsupported writer/controller capabilities, and cancellation/partial-response tests. Do not use `http.TimeoutHandler` or an asynchronous middleware timeout as a substitute.

## Verification and remaining work

Regression tests exercise oversized/small inbound headers, body bytes distinct from header bytes, oversized upstream headers under HTTP/1 and HTTP/2, successful requests after header failures, and response hooks being skipped on header-parse failure. Admission tests cover concurrent saturation, no waiting queue, unread rejected bodies, unchanged request/writer capabilities, normal/panic/cancellation release, completion after unwind, local-response accounting, incremental uploads holding a slot, simultaneous HTTP/2 streams on one connection, and cancellation/replacement while acquiring a saturated upstream pool.

Operational load tests, slow-body enforcement, accepted-connection limits, aggregate process memory/file-descriptor bounds, decompression limits, trailer/informational-response budgets, and long-lived stream/upgrade retirement remain deployment work or later verification passes. These focused tests do not establish production readiness.
