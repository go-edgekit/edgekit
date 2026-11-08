# ADR-0032: Inherit request buffering from the gateway

- **Status:** Accepted
- **Date:** 2026-11-08
- **Supersedes in part:** ADR-0031 (route-only buffering configuration and the meaning of a nil route setting)

Gateways can use the same buffering settings on many routes. Configure that common choice once, but keep an explicit streaming choice for routes with indefinite request streams. `Config.RequestBuffering` supplies an inherited default; a route can replace the complete configuration or select streaming with `NoRequestBuffering()`. This avoids repeated settings without making buffering mandatory for every route.

## Contract

- A nil `Config.RequestBuffering`, or `NoRequestBuffering()` at that level, keeps streaming as the default.
- A nil `Route.RequestBuffering` inherits the gateway setting. Without a gateway setting, routes stream.
- `NoRequestBuffering()` on a route selects streaming, regardless of the gateway setting. It does not disable the body limit or admission.
- An explicit route configuration replaces the complete gateway configuration. Budget and timeout fields are not merged. An incomplete configuration is an error, not a request to inherit individual fields.
- `NoRequestBuffering()` returns a new caller-owned configuration. Adding budget or timeout fields to it is invalid configuration, so conflicting settings cannot silently disable buffering.
- `New` validates the settings of the gateway default even when every route overrides it. Body-limit compatibility is checked for each effective route configuration: buffered routes need a finite effective limit and a budget large enough for it. A gateway with a disabled body limit can supply a buffering default if every route that inherits buffering sets a finite limit.
- `New` copies the effective buffering settings for each route. It shares the configured budget; it does not create a budget for each route. Later changes to the caller's settings do not change the gateway. Callers must not change those settings while `New` reads them.

All request execution, reservation, deadline, replay, cleanup, and failure contracts from ADR-0031 remain unchanged. The gateway default is resolved at construction, not before routing at request time. Matching still fixes the effective body limit and buffering choice before any permitted body read.

## Consequences

Setting a gateway default changes all routes that omit a buffering setting. Shared middleware cannot assume every request is buffered, because an explicit streaming route has no gateway-owned replay. Deployments must size the shared budget for the effective route limits and use separate budgets where cross-route saturation is not acceptable. The existing stored-body reservation bound is not a process-memory bound.

Keyed configuration literals that omit `Config.RequestBuffering` keep the route-only behavior of ADR-0031. A nil route setting differs from that behavior only when a gateway buffering default is set. Positional literals of `Config` or `RequestBuffering` are not compatible with this API change.

## Validation

Tests cover the default, inheritance, explicit streaming, complete overrides, invalid settings without field merging, one report for an invalid gateway default, effective route limits, settings copied at construction, shared-budget saturation across routes, streaming while that budget is full, release after handling, and HTTP/1.1 and HTTP/2 handling. The ADR-0031 buffering tests cover parsing failures, deadlines, replay, cancellation, shutdown, and resource lifetime.
