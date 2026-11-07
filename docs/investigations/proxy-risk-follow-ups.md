# Proxy risk follow-up passes

**Status:** Implementation plan, not an architecture decision or production-readiness claim

The [first proxy-hardening pass](../../README.md#proxy-safety-boundaries) established direct-peer forwarding-header handling, configurable reusable transports, example timeout/pool settings, streaming body limits, and narrower example logs. The following passes address remaining risks without adding gateway business features. ADR statuses and accepted rationale are unchanged.

## 1. Upstream destination restrictions

**Owner:** Optional outbound transport package plus deployment egress controls.

**Initial implementation:** [`extensions/egress`](https://github.com/go-edgekit/edgekit/blob/prototype-final/extensions/egress/README.md) provides an opt-in transport with explicit allowed HTTP(S) origins and IP networks. No universal public-network-only default: private and loopback upstreams are valid when explicitly allowed. Keep the framework's routing and lifecycle API unchanged.

- Validate origin configuration at construction, including scheme, authority, port, and credentials. Compare each outbound URL with the origin allowlist before transport delegation, including on reused connections.
- Resolve at connection time. Validate every address in the lookup result before dialing; reject mixed allowed/denied answers rather than falling back to a denied address.
- Dial a validated IP literal, not the original hostname, to avoid a second DNS lookup between validation and connection. Preserve the request URL, HTTP Host, TLS server name, and certificate verification.
- Apply one connection budget across resolution and connection attempts. Respect cancellation; reuse one owned transport and expose idle-connection cleanup.
- Prevent ambient proxies, custom TLS dial paths, and registered alternate protocols from accidentally bypassing this path. Trusted Go callbacks are not a sandbox boundary.
- Test origin rejection, IPv4/IPv6, mapped addresses, mixed and changing DNS answers, cancellation, TLS identity, proxy bypasses, body closure on rejection, and connection reuse.

**Deployment work still required:** Select narrow origin/network lists, restrict egress outside the process, and review network translation, service meshes, DNS, and credential handling. Existing allowed connections are not revalidated against later DNS answers; immutable policy changes require transport replacement and connection retirement. Forwarding an upstream redirect is not following it, but clients may follow redirects independently.

## 2. Sensitive diagnostics

**Owner:** Framework integration points and executable log sinks.

**Initial implementation:** The [diagnostic audit](sensitive-diagnostics.md) records source-only framework proxy diagnostics, source-only server/standard logging in the example executables, and allowlisted example completion/failure fields. Original records/errors and completion behavior are preserved; no public logging API or lifecycle hook was added.

Continue auditing deployment sinks, including `ReverseProxy` and `http.Server` diagnostics, extension callbacks, startup failures, and panic reporting. Prefer safe error classifications to arbitrary error strings. Preserve original execution errors internally without automatically logging them. Test with credentials, tokens, paths, and query strings embedded in failures. Do not turn extension interpretations into core execution facts or add another lifecycle hook solely for logging.

## 3. Resource limits and slow bodies

**Owner:** Executable, serving edge, and optional synchronous middleware.

**Initial implementation:** The [resource-limit assessment](resource-limits.md) records explicit 64 KiB example inbound/upstream header budgets and optional fail-fast admission, enabled in the example for 64 downstream calls with no waiting queue. Tests cover HTTP/2 stream concurrency, uploads, saturation, cancellation during pool acquisition, and unchanged completion behavior. Slow-body enforcement remains deployment-owned.

Continue selecting workload-specific inbound-header and upstream-response-header limits. Bound concurrent requests and queued work; connection counts do not bound HTTP/2 streams. Apply slow-upload and slow-reader policies per workload, preferably at the serving edge. Preserve legitimate long-lived streams; do not introduce blanket response deadlines, `http.TimeoutHandler`, or asynchronous middleware. Verify cancellation while queued and under saturation.

## 4. Oversized upload semantics

**Owner:** Upstream application and application-specific upload protocol.

Streaming byte limits are not atomic rejection. An upstream may receive part of an oversized request. Require safe handling of truncated uploads and defer irreversible effects until complete validation. Where atomic uploads are required, use explicit staging and cleanup, not default gateway buffering. Test partial uploads, early upstream responses, cancellation, and cleanup.

## 5. Operational verification

**Owner:** Deployment tests and operational configuration.

Exercise slow clients, disconnects, sustained upstream failures, HTTP/2 concurrency, protocol upgrades, and shutdown with active streams. Measure connection counts, queued work, goroutines, memory, and file descriptors under saturation. Define how long-lived connections drain or terminate. Keep Go patched and repeat protocol/load tests after relevant changes.

## Verification for each implementation pass

Add positive and negative regression tests for the behavior changed. Run `go test ./...`, `go test -race ./...`, and `go vet ./...`. Record established protections separately from deferred risks. Do not change ADR statuses or claim production readiness based on a focused pass.
