# ADR-0011: Build a library-first gateway framework with a correct core and policy in extensions

- **Status:** Accepted
- **Date:** 2026-11-01
- **Decision owners:** Project maintainers
- **Supersedes:** [ADR-0001](0001-framework-scope.md)

## Context

The project started from repeated limitations in Tyk caused by its opinionated model: limits apply only per key, rate-limit and quota policies cannot be defined separately because the lowest limit wins, and few rate-limit algorithms exist. The goal is to take the opposite approach: provide building blocks that could, together with extensions, replace such a product.

ADR-0001 described the core as minimal. That word was ambiguous: it could mean minimal features or minimal safety. The current code is a prototype for exploring interactions, not the intended shape.

## Decision

Build an embeddable Go **library** first. Its core serves, routes, and proxies HTTP and owns HTTP correctness and hardening. The core contains no business policy: no customers, keys, credentials, rate limits, quotas, billing, or storage schemas.

The project follows three guiding principles:

1. **Pingora:** proxy execution is a set of named phases with a per-request context (ADRs 0003 and 0005).
2. **pi:** a small core where extensions are as capable as built-in features. Features stay out of the core until an extension proves the need.
3. **Ghostty:** library first, so every runnable gateway is one consumer of the library; strict correctness over convenience.

Business policy is offered as **bundled extensions** maintained with the project, such as key authentication, rate limits, quotas, transformations, and analytics. Bundled extensions use only the public contract and get no private access to the core. They serve as the evidence that the public contract is sufficient.

A **distribution** is a runnable gateway assembled from the core and chosen extensions, with its own configuration format and operational defaults. Distributions consume the framework and are not part of it. A distribution, including declarative configuration and an extension registry, requires its own decision later.

The project should be usable as open source. Replacing Tyk at work is a guiding use case, not a commitment; Tyk-format compatibility, if ever needed, belongs in a separate importer extension.

## Consequences

**Positive:** Clear core boundary; the Tyk limitations become acceptance scenarios for extensions; bundled extensions continuously test the public API.

**Negative:** The core is larger than a bare proxy because it includes hardening (see ADR-0012). Users must assemble a binary until a distribution exists.

## Alternatives considered

- **Build an API-management product:** Repeats the opinionated model this project exists to avoid.
- **A bare proxy core with all hardening in optional packages:** Makes the safe configuration the hard one.
- **Privileged built-in policy features:** Hides gaps in the public contract.

## Validation / follow-up

Each Tyk limitation listed above must be solvable by a bundled or user extension without changing core source. The prototype is replaced by a rewrite that uses the current tests as a behavior specification (see the [roadmap](../roadmap.md)).
