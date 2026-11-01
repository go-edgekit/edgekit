# ADR-0012: Ship secure defaults and configurable protections in the core

- **Status:** Accepted
- **Date:** 2026-11-01
- **Decision owners:** Project maintainers
- **Supersedes:** [ADR-0010](0010-scaffold-hardening-and-verification.md)

## Context

ADR-0010 framed hardening as verification of a scaffold, and the prototype placed body limits, admission, and egress restrictions in optional packages. The [proxy implementation survey](../investigations/proxy-implementation-survey.md) found correctness risks (path confusion, query semicolons, header aliasing) and left open whether such protections are framework defaults or opt-in middleware.

## Decision

Hardening and HTTP correctness are core responsibilities, not extensions. The core classifies each protection by one rule:

- **Secure default:** on without configuration, because a safe value exists for nearly every workload. Users may deliberately relax it.
- **Configurable protection:** provided by the core but off until configured, because a safe value depends on the deployment.

Initial classification:

| Secure defaults | Configurable protections |
| --- | --- |
| Path policy, reject mode (R1); query-semicolon rejection (R2); header-name alias rejection (R3); removal of untrusted identity headers (R4); 504/499 error mapping (R5); `Alt-Svc` stripping (R7); transport timeouts; header budgets; no ambient environment proxy; direct-peer forwarding headers | Egress origin and network allowlists; trusted-proxy networks (R6); admission capacity; slow-body idle deadlines (R9) |

The request-body limit is a secure default with a conservative value. Routes that accept large uploads must override it explicitly.

Relaxing a secure default is an explicit configuration choice, never the result of omitting a value. Body limits, admission, and egress move from optional packages into the core as protections.

The project still does not claim production readiness until requirements and tests justify it. Each protection needs positive and negative tests, and `go test ./...`, `go test -race ./...`, and `go vet ./...` must pass.

## Consequences

**Positive:** The zero-configuration gateway is the safe one; correctness fixes reach every user; ownership of each protection is explicit.

**Negative:** A larger core; some defaults will reject traffic that legacy upstreams accept, which requires clear documentation of how to relax them.

## Alternatives considered

- **All protections opt-in (Envoy style):** Maximum compatibility, but unsafe by default.
- **Hardening as bundled extensions:** Treats correctness as policy and allows a gateway without it.

## Validation / follow-up

R6 and R8–R11 remain on the [roadmap](../roadmap.md) after the bundled policy extensions. Reclassify a protection only with evidence about workloads.
