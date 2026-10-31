# ADR-0006: Start with compile-time Go extensions, not dynamic plugins

- **Status:** Proposed
- **Date:** 2026-10-31
- **Decision owners:** Project maintainers

## Context

We want a potentially open-source, highly customizable gateway. "Extensible" does not inherently require installing code into an already-running binary. Dynamic plugins can complicate compilation, compatibility, security, deployment, and lifecycle management.

## Decision

For the initial framework, extensions are ordinary Go packages. Users import packages, construct extensions with their configuration/dependencies, register the chosen hooks/middleware, and compile a gateway executable.

- Extension configuration and resource ownership belong to the extension or example executable.
- Extension initialization occurs before serving requests.
- The gateway does not require a central plugin registry, dynamic code loading, WASM, or gRPC plugin processes.
- Prefer small optional hook interfaces and ordinary Go functions over large framework-specific base classes.
- Treat public extension APIs as experimental until validated with independently authored extensions.

## Consequences

**Positive:** Type-checked integration; simple local development; no runtime plugin ABI; easy Go tests; no mandatory plugin runtime.

**Negative:** Adding or upgrading an extension requires rebuilding/deploying the binary; third-party extension API changes require normal Go module version management.

## Alternatives considered

- **Go `plugin`:** Platform/compatibility restrictions and complex build requirements make it unsuitable as the starting point.
- **WASM/sidecar/gRPC plugins:** Potentially useful for multi-language or independently deployed extensions, but add contracts and operational complexity.

## Validation / follow-up

Build an example extension entirely in a separate package and confirm it can be combined with another extension. Revisit dynamic loading only when there is a documented requirement.
