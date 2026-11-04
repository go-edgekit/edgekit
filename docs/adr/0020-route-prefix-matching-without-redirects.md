# ADR-0020: Match route prefixes exactly and send no redirects

- **Status:** Accepted
- **Date:** 2026-11-03
- **Decision owners:** Project maintainers

## Context

ADR-0008 separates route matching from upstream selection. It does not specify how routes match.

Trailing-slash redirects are a common router feature. `http.ServeMux` redirects `/api` to `/api/` when only the subtree `/api/` is registered, and HttpRouter makes the redirect configurable with `RedirectTrailingSlash`. They are not a reverse-proxy requirement.

## Decision

- A route prefix matches the escaped request path. The match is case-sensitive, and the longest matching prefix wins.
- A prefix starts and ends with `/`. The prefix `/api/` matches `/api/` and `/api/users`, but not `/api`.
- The core never sends a redirect. A path that no route matches is a routing miss: 404 Not Found, with the rejection reason `no_route` at completion ([ADR-0018](0018-completion-observes-gateway-rejections.md)).

A redirect is a local response that makes a claim about the URL space of the upstream: `/api` and `/api/` can be different resources there. An embedder who wants the redirect adds a route or middleware for it.

## Consequences

**Positive:** Routing has no hidden local responses. A routing miss is one explicit fact.

**Negative:** Clients that expect the redirect that `http.ServeMux` sends get 404.

## Alternatives considered

- **Redirect like `http.ServeMux`:** Familiar to Go users, but it answers for the upstream.
- **A configurable redirect:** Possible later as a route option, if a requirement needs it.

## Validation / follow-up

Tests verify the missing trailing slash as a routing miss with completion, next to the other routing-miss cases.
