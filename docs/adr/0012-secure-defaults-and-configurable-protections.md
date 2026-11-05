# ADR-0012: Ship secure defaults and configurable protections in the core

- **Status:** Accepted
- **Date:** 2026-11-01
- **Decision owners:** Project maintainers
- **Supersedes:** [ADR-0010](0010-scaffold-hardening-and-verification.md)

## Context

ADR-0010 framed hardening as verification of a scaffold, and the first implementation (tag `prototype-final`) placed body limits, admission, and egress restrictions in optional packages. The [proxy implementation survey](../investigations/proxy-implementation-survey.md) found correctness risks (path confusion, query semicolons, header aliasing) and left open whether such protections are framework defaults or opt-in middleware.

## Decision

Hardening and HTTP correctness are core responsibilities, not extensions. The core classifies each protection by one rule:

- **Secure default:** on without configuration, because a safe value exists for nearly every workload. Users may deliberately relax it.
- **Configurable protection:** provided by the core but off until configured, because a safe value depends on the deployment.

Initial classification:

| Secure defaults | Configurable protections |
| --- | --- |
| Path policy, reject mode (R1); query-semicolon rejection (R2); header-name alias rejection (R3); removal of untrusted identity headers and extension-managed headers (R4); 504/499 error mapping (R5); `Alt-Svc` stripping (R7); rejection of opaque URIs, absolute-form targets, and `CONNECT`; no transparent transport compression; transport timeouts; header budgets and HTTP/2 limits (R8); progress-based idle deadlines (R9); no ambient environment proxy; direct-peer forwarding headers | Egress origin and network allowlists; trusted-proxy networks (R6); admission capacity |

The request-body limit is a secure default with a conservative value. Routes that accept large uploads must override it explicitly.

Further rules:

- **Normalize mode** for the path policy forwards the normalized path to the upstream, never the original encoding.
- **Transport compression:** the core forwards the client's `Accept-Encoding` unchanged and never decompresses upstream responses on its own initiative.
- **Managed headers:** an extension may declare headers it owns, such as a header that tells the upstream the authenticated customer. The core removes these headers from inbound requests, in the same step as untrusted identity headers, so no extension needs its own stripping.
- **Idle deadlines (R9)** reset on every read or write progress and use a generous default (about 300s). Streaming routes may relax them. They require the core-owned server ([ADR-0016](0016-core-owns-the-inbound-server.md)).

Relaxing a secure default is an explicit configuration choice, never the result of omitting a value. Body limits, admission, and egress move from optional packages into the core as protections.

The project still does not claim production readiness until requirements and tests justify it. Each protection needs positive and negative tests, and `go test ./...`, `go test -race ./...`, and `go vet ./...` must pass.

## Consequences

**Positive:** The zero-configuration gateway is the safe one; correctness fixes reach every user; ownership of each protection is explicit.

**Negative:** A larger core; some defaults will reject traffic that legacy upstreams accept, which requires clear documentation of how to relax them.

## Alternatives considered

- **All protections opt-in (Envoy style):** Maximum compatibility, but unsafe by default.
- **Hardening as bundled extensions:** Treats correctness as policy and allows a gateway without it.

## Validation / follow-up

R5 uses typed dial and request-written failures (R10), never error strings. R6 and R11 are planned in the [roadmap](../roadmap.md).

The classification was extended on 2026-11-01, after reviewing the [proxy implementation survey](../investigations/proxy-implementation-survey.md) against this ADR: managed headers, opaque/absolute-form/`CONNECT` rejection, and transport compression were added, and R9 moved from configurable protection to secure default. Reclassify a protection only with evidence about workloads.

**Refined 2026-11-03:** [ADR-0017](0017-failure-status-by-source.md) sets the status of the failures that R5 does not cover, by the source of the failure.

**Refined 2026-11-04:** [ADR-0023](0023-failure-evidence-and-transport-facts.md) defines the evidence for R5 and the parts of R10 that the core provides.

**Refined 2026-11-05:** [ADR-0024](0024-hardened-server-deadlines-and-upgraded-connections.md) defines how a caller disables a server timeout, how a streaming route relaxes the idle deadlines (R9), and how the deadlines measure progress.
