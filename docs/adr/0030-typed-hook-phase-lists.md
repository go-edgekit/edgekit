# ADR-0030: Register typed hook lists in execution order

- **Status:** Accepted
- **Date:** 2026-11-07
- **Decision owners:** Project maintainers
- **Supersedes:** [ADR-0021](0021-hook-order-by-provider-position.md)
- **Supersedes in part:** [ADR-0029](0029-access-log-extension-contract.md), provider-position registration only

## Context

With ADR-0021, `Config.Hooks []any` accepted values that did not supply a lifecycle hook. The core discovered capabilities at startup. One position determined the order of every phase: request phases ran forward, and response and completion phases ran backward. A provider that implemented another hook interface also joined that phase without a configuration change.

ADR-0021 keeps the request and response order of one provider consistent. This helps a provider that signs requests and verifies responses. It does not give a provider an entry and exit scope: processing phases can be skipped, and completion runs even when a processing hook did not run. Middleware already provides handler nesting.

Other proxies use both models. Envoy and Skipper use forward request order and reverse response order. GoProxy has separate typed request and response registrations, both in registration order. Pingora exposes named lifecycle callbacks; its reusable HTTP modules use the same priority order for request and response callbacks. These designs show that response reversal is a choice, not a requirement of lifecycle hooks.

## Decision

`Config.Hooks` has type `Hooks`, with four independent typed lists:

- `RequestReceived []RequestReceivedHook`
- `BeforeUpstream []BeforeUpstreamHook`
- `UpstreamResponse []UpstreamResponseHook`
- `RequestCompleted []RequestCompletedHook`

Each list runs serially in its written order. There is no automatic reversal, capability discovery, or relationship between positions in different lists. The handling stages stay in their fixed order. A value runs only in the phase lists that contain it. Repeated entries run once per occurrence.

The zero value and empty lists are valid. `New` rejects every nil entry, including typed nil receivers and nil function adapters, with an error that wraps `ErrInvalidConfiguration`. Errors identify the phase and index, not configuration values. The compiler rejects values that do not implement the interface of the list.

`New` copies each list into the runtime configuration. It does not call hook methods during validation. The gateway shares the hook values between requests, so the values must be safe for concurrent use. Rejections, failures, completion, and request-state lifetimes keep their existing contracts.

Managed-header declarations are separate. `Config.HeaderOwners []HeaderOwner` contains the declarations. `Config.ManagedHeaders []string` stays available. For each header-owner entry, `New` calls `ManagedHeaders` once and copies the names into the managed-header set. Nil entries, invalid names, and duplicates within one declaration are errors. Declarations can overlap with other declarations and with `ManagedHeaders`. A hook that also implements `HeaderOwner` must be registered separately in `HeaderOwners`. A header owner does not register lifecycle hooks.

The access-log recorder is registered as global middleware, in `BeforeUpstream` before hooks that read, change, or sign its request-ID header, and last in `UpstreamResponse` and `RequestCompleted`. It is also registered in `HeaderOwners`. This preserves the observation contract of ADR-0029 without provider nesting. The signer is last in `BeforeUpstream` and is a separate header owner.

## Consequences

Configuration states the execution order directly and checks hook types at compile time. Adding a method to a provider does not silently add a phase or a header declaration.

A provider with several phases needs several registrations. Callers must coordinate cross-phase dependencies. A request signer that also verifies responses must be late in the request list and early in the response list. A missed header-owner registration means the declared names are not removed, unless `ManagedHeaders` or another header owner also declares them. Bundled examples register every owner explicitly.

This is a source-breaking API change. To preserve the old execution order during migration, expand the old provider list into the request lists in forward order and the response and completion lists in reverse order. Move all header declarations to `HeaderOwners`. The element type of each list replaces the tests that supplied an unrelated value to `[]any`: the compiler rejects such a value. Nil-entry and header-validation tests remain runtime tests.

There is no new dependency, protocol deviation, or change to the trust assigned to configuration and extensions. The core checks headers after hooks as before. Registration and compilation remain startup work.

## Alternatives considered

- **A typed registration struct for each provider:** Keeps capabilities together and preserves automatic reversal, but configuration still expresses provider positions rather than phase execution.
- **A marker interface:** Prevents arbitrary values but does not check lifecycle capabilities or expose phase order.
- **One interface with every phase:** Requires no-op methods for phases that a provider does not need.
- **Numeric priorities:** Hide the effective order and require unrelated extensions to coordinate numbers.

## Validation

Tests cover independent list order, repeated entries, registration of only the listed phases, list copies, empty lists, nil entries in every phase, failure and rejection short-circuiting, and separate header-owner registration. Header-owner tests cover validation, overlapping declarations, one startup call, copied names, and no implicit hook registration. Existing cancellation, concurrency, panic, body-closure, and completion tests remain in place.

## Sources

Sources were read at pinned upstream commits, not executed:

- [Envoy ordering](https://github.com/envoyproxy/envoy/blob/082be7bec2dd5553b826eaa602cafea23d071399/docs/root/intro/arch_overview/http/http_filters.rst#L49-L68)
- [Skipper request and response execution](https://github.com/zalando/skipper/blob/62cfb66bb7327d1742f623a3804ff0a11f82c14f/proxy/proxy.go#L992-L1050)
- [GoProxy typed lists and execution](https://github.com/elazarl/goproxy/blob/e7a00c638cf0ea55b2d51ca8b9c99383fc42382a/proxy.go#L94-L116)
- [Pingora HTTP module ordering](https://github.com/cloudflare/pingora/blob/1adcb3815b70d6594379db044b5aa67f9bc1af76/pingora-core/src/modules/http/mod.rs)
