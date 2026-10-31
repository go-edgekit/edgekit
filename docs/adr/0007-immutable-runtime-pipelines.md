# ADR-0007: Separate configuration from an immutable runtime pipeline

- **Status:** Proposed
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

A gateway serves many concurrent requests. It should not repeatedly parse configuration, assemble middleware, or resolve extension registration on the hot path. Mature gateways also need validation and eventually safe configuration reloads.

## Decision

Separate **declarative startup configuration** from a **compiled runtime snapshot**:

1. Accept a minimal configuration describing routes, upstream targets, and registered extension instances.
2. Validate it up front (e.g., duplicate routes, unsupported schemes, invalid upstream destinations).
3. Prepare route matchers, transports, and ordered extension pipelines at initialization.
4. Serve requests using the resulting immutable runtime snapshot.

Treat route definitions and pipeline membership as immutable during normal request processing. Any mutable counters or caches live inside explicitly owned extension instances, not in the route configuration.

The scaffold does **not** require dynamic configuration reload, an admin API, a database, or a control plane.

## Consequences

**Positive:** Less hot-path work, simpler concurrent access, testable initialization, possible future atomic snapshot swapping.

**Negative:** Changing routes or enabled extensions initially requires restarting; configuration must be validated consistently.

## Alternatives considered

- **Read configuration on every request:** Wasteful and harder to reason about.
- **Build full control plane and hot reload immediately:** Unnecessary for the first architecture experiment.

## Validation / follow-up

Test invalid startup configurations and concurrent requests through a compiled gateway. If hot reload is added later, build and validate a new snapshot before publishing it, and let in-flight requests retain their snapshot.
