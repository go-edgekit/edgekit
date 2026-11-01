# ADR-0014: Keep the core dependency-light and give bundled extensions their own modules

- **Status:** Accepted
- **Date:** 2026-11-01
- **Decision owners:** Project maintainers

## Context

The core uses only the Go standard library. Bundled extensions will need third-party packages, such as Redis clients, JWT libraries, or OpenTelemetry. A single module would make every user of the core download those dependencies.

## Decision

The repository contains several Go modules. The core module keeps third-party dependencies minimal; the standard library and `golang.org/x` are preferred, and any other dependency needs a clear reason. Each bundled extension that has third-party dependencies is its own module in the same repository. Extensions without such dependencies may stay in the core module.

The core provides no generic store or sink abstraction. Each bundled extension defines the narrow interface it needs, such as a rate-limit counter store or an analytics sink, and adapters for specific systems are separate modules.

The module path changes from `example.com/go-gateway-starter` to a real public path during the rewrite, together with an Apache-2.0 license.

## Consequences

**Positive:** Users of the core avoid unused dependencies; extensions version independently; storage choices stay with the extension that needs them.

**Negative:** Multi-module release and versioning work; a cross-module change needs coordinated releases.

## Alternatives considered

- **One module with all dependencies:** Simple releases, heavy core.
- **A core key-value store interface:** Repeats the Tyk model of one shared store for everything and cannot fit each extension's consistency needs.
