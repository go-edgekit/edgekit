# Roadmap

**Status:** Plan from the 2026-11-01 direction review. Easy to change; decisions live in the [ADRs](adr/README.md).

The current code is a prototype. It is replaced by a rewrite, not evolved. The existing tests are the behavior specification for the rewrite: lifecycle, proxy safety, access-log, egress, admission, and body-limit tests carry over in intent.

## 1. Settle direction

Done: ADRs 0011–0016, status changes for 0002 and 0006–0008, and the [context map](../CONTEXT-MAP.md).

## 2. Rewrite the core

- Public module path and Apache-2.0 license ([ADR-0014](adr/0014-multi-module-repository.md)). Done: `github.com/go-edgekit/edgekit` with the Apache-2.0 license.
- Secure defaults R1–R5, R7, R8, and R9; opaque/absolute-form/`CONNECT` rejection; no transparent transport compression; extension-managed headers; body limit, admission, and egress as core protections ([ADR-0012](adr/0012-secure-defaults-and-configurable-protections.md)).
- Core-owned hardened server ([ADR-0016](adr/0016-core-owns-the-inbound-server.md)).
- Typed dial and request-written failures (R10) as execution facts, used by error mapping.
- Upstream-selector extension point with static selection ([ADR-0008](adr/0008-separate-routing-from-upstream-selection.md)).
- Typed rejections and replaceable `problem+json` renderer ([ADR-0013](adr/0013-typed-rejections-and-replaceable-renderer.md)).
- Path and query rewriting on routes; routes no longer always forward the original path.
- Rename the hook-provider registration so "extension" keeps its glossary meaning.

## 3. Bundled access-policy extensions

Proves the motivating limitations can be solved without core changes ([ADR-0015](adr/0015-access-policy-model.md)):

1. Key authentication with hashed key store and in-memory adapter.
2. Rate limits (GCRA, sliding-window counter) and quotas (calendar periods) with a static limit source and in-memory store.
3. Redis store adapter as a separate module.
4. Header transformations. Body transformations need their own ADR.

**Open experiment:** how route-specific settings reach an extension. The working hypothesis is route bindings (register once, bind per route, per-route hook lists compiled at startup). Route middleware with shared request state is the fallback. Decide from the rate-limit extension, then record an ADR.

## 4. Remaining hardening and analytics

- R6 trusted-proxy mode, R11 idle-connection retirement.
- Analytics: the access-log extension with a sink interface; exporters as separate modules.

## Deferred

Distribution with declarative configuration, configuration hot reload, bundled load balancing and health checks, body transformations, importer for other gateway configuration formats.
