# Rewrite ledger

**Status:** Working document for the core rewrite. Delete it, together with the rewrite line in [AGENTS.md](../AGENTS.md), when every row is covered or dropped.

The prototype is kept at the git tag `prototype-final`. Its tests are the behavior specification for the rewrite. The rewrite does not port them. It writes new tests against the new API that examine the same conditions.

## Rules

- Each prototype test has one row. Remove a prototype test only in the same change that sets its row to _covered_ or _dropped_, or as the next rule permits.
- A slice can remove a prototype test together with the code that the test needs, when the slice removes or breaks that code. The row stays `pending`. The test stays readable at `prototype-final`.
- When parts of one test belong to different slices, split the row. Each part gets its own row in its slice, marked _(split)_.
- _Covered_: list the new tests that examine the same conditions. Test names can change. The conditions cannot.
- _Dropped_: name the ADR that removes or changes the behavior. If a test shows that an accepted ADR is wrong, stop and write a superseding ADR.
- When a new test differs from the prototype behavior because of an ADR, write the old and new behavior in the row.
- Write new tests as black-box tests through the public API. A white-box test is permitted only for a unit without a public surface. Give the reason in the row.
- `internal/safelog` is not part of the rewrite and has no rows.

The headings give the planned slice for each test (see the [roadmap](roadmap.md)). A row can move to a different slice when the work shows a better fit.

Status values: `pending`, `covered`, `dropped`.

## Slice 1: proxy core

| Prototype test | Behavior | Status | New tests | ADR |
| --- | --- | --- | --- | --- |
| `gateway_test.go:TestProxyForwardsRequestAndAppliesRouteMiddleware` | A matched route forwards the request upstream and runs its route middleware | covered | `gateway_test.go:TestProxyForwardsRequestAndAppliesRouteMiddleware`, `gateway_test.go:TestRouteMatching`, `gateway_test.go:TestRouteMiss` | 0008 |
| `gateway_test.go:TestGlobalAndRouteMiddlewareOrder` | Global middleware wraps route middleware in registration order | covered | `gateway_test.go:TestGlobalAndRouteMiddlewareOrder`, `gateway_test.go:TestGlobalMiddlewareRunsForRouteMiss` | 0004 |
| `gateway_test.go:TestInvalidRoute` | Construction rejects invalid route configuration | covered | `gateway_test.go:TestInvalidConfiguration`, `gateway_test.go:TestInvalidConfigurationReportsAllProblems`, `gateway_test.go:TestMiddlewareIsCalledOnceForValidConfiguration`, `gateway_test.go:TestMiddlewareIsNotCalledForStructuralProblem`, `gateway_test.go:TestStaticUpstreamAcceptsValidURL`, `gateway_test.go:TestStaticUpstreamRejectsInvalidURL`, `gateway_test.go:TestConfigurationIsCopied` | 0007 |
| `proxy_safety_test.go:TestForwardingTrustAndHopByHopHeaders` | Inbound forwarding headers are not trusted; hop-by-hop headers are removed | covered | `inbound_test.go:TestForwardingTrustAndHopByHopHeaders`, `inbound_test.go:TestGatewayDoesNotChangeInboundRequest` | 0012 |
| `proxy_safety_test.go:TestTransportReusesConnectionsAcrossRoutes` | Routes to the same upstream share one connection pool | covered | `transport_test.go:TestTransportReusesConnectionsAcrossRoutes`, `transport_test.go:TestCloseIdleConnections` | |
| `proxy_safety_test.go:TestResponseHeaderTimeoutCancelsUpstream` | The response-header timeout cancels the upstream request | covered | `transport_test.go:TestResponseHeaderTimeoutCancelsUpstream` | 0012 |
| `proxy_safety_test.go:TestCancellationWhileWaitingForConnectionPool` | Client cancellation ends a wait for a pooled connection | covered | `transport_test.go:TestCancellationWhileWaitingForConnectionPool` | |
| `proxy_safety_test.go:TestRequestBodyStreamsBeforeClientEOF` | The request body streams upstream before the client finishes sending | covered | `transport_test.go:TestRequestBodyStreamsBeforeClientEOF` | 0002 |
| `cmd/gateway/main_test.go:TestUpstreamTransportPolicy` | The upstream transport has bounded timeouts and pool settings | covered | `transport_test.go:TestUpstreamTransportPolicy`, `transport_test.go:TestNoTransparentCompression` | 0012 |
| `cmd/gateway/main_test.go:TestHeaderTimeoutDoesNotLimitResponseStream` | The response-header timeout does not cut a streaming response body | covered | `transport_test.go:TestHeaderTimeoutDoesNotLimitResponseStream` | 0012 |
| `cmd/gateway/main_test.go:TestUpstreamDialRespectsCanceledContext` | The upstream dial stops when the request context is canceled, also before a connection exists | covered | `transport_test.go:TestUpstreamDialRespectsCanceledContext` | |
| `diagnostics_test.go:TestGatewayConstructionDoesNotReconfigureProcessLogging` | Gateway construction does not change process-global logging | covered | `gateway_test.go:TestGatewayConstructionDoesNotReconfigureProcessLogging` | |

Changes from the prototype behavior:

- `TestForwardingTrustAndHopByHopHeaders`. Old: the gateway removed only `Forwarded`, `X-Forwarded-For`, `-Host`, `-Proto`, and `X-Real-IP`. Other identity headers, such as `X-Forwarded-Port` and `True-Client-IP`, reached the upstream, and `Alt-Svc` reached the client. New: the gateway removes all identity headers and the configured managed headers before middleware runs, removes their names from `Connection`, and removes `Alt-Svc` and hop-by-hop fields from responses, informational responses, and trailers (ADR-0012, R4 and R7). Middleware must not change the request that it gets, as `http.Handler` requires; the gateway does not change the caller's request.
- `TestUpstreamTransportPolicy`. Old: the bounded transport was example code in `cmd/gateway`, with `MaxConnsPerHost` 64. A gateway without a configured transport used `http.DefaultTransport`, with the environment proxy and transparent compression. New: `NewTransport` is the default transport of every gateway. `MaxConnsPerHost` is 0 (no limit), because a limit makes requests wait for the pool with no bound except the client context. Admission (slice 5) limits concurrency. Compression is off, and `New` rejects an `*http.Transport` with compression (ADR-0012).
- `TestInvalidRoute`. Old: errors contained the route prefix. New: errors wrap `ErrInvalidConfiguration`, give the location only, and report all problems together. `New` calls no middleware function when the structure of the configuration is not valid.
- `TestResponseHeaderTimeoutCancelsUpstream` and `TestRequestBodyStreamsBeforeClientEOF` are split. The completion part and the admission part are rows in slices 2 and 5.

New tests without a prototype row: `inbound_test.go:TestSecureDefaultsRejectBeforeMiddleware` and `inbound_test.go:TestRequestFromOutsideServerNeedsOriginForm` (request-target rejection, `CONNECT`, R1 to R3), `inbound_test.go:TestQueryIsForwardedUnchanged`, `inbound_test.go:TestMiddlewareQueryIsForwardedUnchanged`, `inbound_test.go:TestConnectionCannotRemoveManagedHeader`, `inbound_test.go:TestRequestTrailersCannotCarryRemovedHeaders`, `response_test.go:TestInformationalResponseHeadersAreSanitized`, `response_test.go:TestInformationalResponseKeepsMiddlewareHeaders`, `response_test.go:TestTrailersAreSanitized`, `response_test.go:TestProtocolUpgradeIsForwarded`, `gateway_test.go:TestCustomUpstreamSelector` (ADR-0008), and the `Example` functions in `example_test.go`.

## Slice 2: lifecycle hooks and request state

| Prototype test | Behavior | Status | New tests | ADR |
| --- | --- | --- | --- | --- |
| `lifecycle_test.go:TestLifecycleContextAndOrder` | Hooks and middleware share one gateway context; phases run in a fixed order | covered | `lifecycle_test.go:TestLifecycleContextAndOrder` | 0003, 0004, 0005, 0021 |
| `lifecycle_test.go:TestCompletionOnEarlyExits` | Completion runs when middleware or a hook ends handling early | covered | `lifecycle_test.go:TestCompletionOnEarlyExits`, `lifecycle_test.go:TestSecureDefaultRejectionReachesCompletion` | 0009, 0018, 0020 |
| `lifecycle_test.go:TestHookErrorsStopPhaseAndHideInternalDetails` | A hook error stops its phase; the response contains no internal details | covered | `lifecycle_test.go:TestHookErrorsStopPhaseAndHideInternalDetails`, `outbound_test.go:TestBeforeUpstreamInvalidHeaderFails` | 0009, 0017 |
| `lifecycle_test.go:TestTransportErrorReachesCompletion` | A transport error is an execution fact visible at completion | covered | `lifecycle_test.go:TestTransportErrorReachesCompletion` | 0009 |
| `lifecycle_test.go:TestCompletionDuringPanicUnwinding` | Completion runs while a panic unwinds, and the panic continues | covered | `lifecycle_test.go:TestCompletionDuringPanicUnwinding` | 0009 |
| `lifecycle_test.go:TestStreamingFailureAbortsAndCompletes` | A failure during response streaming aborts the response and completion runs | covered | `lifecycle_test.go:TestStreamingFailureAbortsAndCompletes` | 0009 |
| `lifecycle_test.go:TestRequestTagExtension` | Middleware and hooks of one extension share typed request state | covered | `lifecycle_test.go:TestExtensionSharesStateBetweenMiddlewareAndHooks` | 0005 |
| `lifecycle_test.go:TestTypedStateIsolationAndNilValues` | Typed state keys are isolated from each other; nil values are stored and read correctly | covered | `lifecycle_test.go:TestTypedStateIsolationAndNilValues`, `lifecycle_test.go:TestStateIsDroppedAfterCompletion` | 0005 |
| `lifecycle_test.go:TestConcurrentRequestStateIsolation` | Concurrent requests do not see each other's state | covered | `lifecycle_test.go:TestConcurrentRequestStateIsolation` | 0005 |
| `lifecycle_test.go:TestCancellationReachesHooks` | Client cancellation is visible to hooks | covered | `lifecycle_test.go:TestCancellationReachesHooks` | 0005 |
| `lifecycle_test.go:TestCompletionOnOutboundPanic` | Completion runs when an outbound hook panics | covered | `lifecycle_test.go:TestCompletionOnOutboundPanic` | 0009 |
| `lifecycle_test.go:TestInvalidExtensions` | Construction rejects invalid hook providers | covered | `gateway_test.go:TestInvalidHookProviders`, `gateway_test.go:TestHookProvidersAccepted` | 0007 |
| `outbound_test.go:TestOutboundTransportClonesMetadataAndPassesTheHookRequest` | Outbound hooks get a copy of request metadata; the transport gets the request that the hooks made; the body streams | covered | `outbound_test.go:TestBeforeUpstreamHookChangesReachTransport`, `outbound_test.go:TestBeforeUpstreamHeaderRules` | 0003, 0019 |
| `outbound_test.go:TestOutboundTransportFailureClosesBodyWithoutDelegating` | An outbound hook failure stops later hooks, closes the body, and does not call the transport | covered | `outbound_internal_test.go:TestOutboundFailureClosesBodyWithoutDelegating` (white-box, see below), `lifecycle_test.go:TestHookErrorsStopPhaseAndHideInternalDetails` | 0003 |
| `proxy_safety_test.go:TestConfiguredTransportWithAndWithoutOutboundHooks` | A configured transport is used with and without outbound hooks | covered | `outbound_test.go:TestConfiguredTransportWithAndWithoutOutboundHooks` | 0003, 0017 |
| `proxy_safety_test.go:TestClientCancellationReachesUpstreamAndCompletion` | Client cancellation reaches the upstream and is visible at completion | covered | `lifecycle_test.go:TestClientCancellationReachesUpstreamAndCompletion` | 0009 |
| `proxy_safety_test.go:TestResponseHeaderTimeoutCancelsUpstream` _(split)_ | A response-header timeout is a timeout error at completion, and completion runs after middleware unwinds | covered | `lifecycle_test.go:TestResponseHeaderTimeoutReachesCompletion` | 0009 |
| `cmd/gateway/resource_limits_test.go:TestUpstreamHeaderLimitBeforeResponseHooks` | Oversized upstream response headers fail before response hooks run, on HTTP/1.1 and HTTP/2 | covered | `transport_test.go:TestUpstreamHeaderLimitBeforeResponseHooks` | 0012, 0017 |

Changes from the prototype behavior:

- Registration and outcome. Old: `Config.Extensions []any`, hook methods `OnRequest`, `OnBeforeUpstream`, `OnUpstreamResponse`, and `OnComplete`, and `Outcome{Duration, Err, Aborted}`. New: `Config.Hooks []any` with `RequestReceivedHook`, `BeforeUpstreamHook`, `UpstreamResponseHook`, `RequestCompletedHook`, and `HeaderOwner`. Completion gets `Completion`: start, duration, route, selected upstream, the first failure with its phase, the rejection reason, and the abort fact. Construction errors wrap `ErrInvalidConfiguration` and give the location `hooks N` only.
- `TestCompletionOnEarlyExits`. Old: `/api` redirected to `/api/` (`http.ServeMux`), and a hook rejection with `HTTPError` gave 429. New: `/api` does not match `/api/`, gets 404, and completion runs with the reason `no_route` (ADR-0020). A hook error gives 500; the typed rejection is a slice-3 row. Completion also runs for secure-default rejections, with a reason code and no request content (ADR-0018).
- `TestHookErrorsStopPhaseAndHideInternalDetails`. Old: 500 for a request-hook error, 502 for outbound and response hooks; `HTTPError` set the status and message. New: 500 for all three phases, because the failure is in the extension, not in the upstream (ADR-0017). The `HTTPError` cases are a slice-3 row.
- `TestOutboundTransportClonesMetadataAndPassesTheHookRequest`. Old: the hook got a clone of the `*http.Request` and could change its URL and body. New: the hook gets an `OutboundRequest`, whose only mutable part is the header map, so it cannot change the destination (ADR-0019). The hook changes the header map of the outbound copy that `ReverseProxy` makes, so the inbound request does not change; the core makes no second clone. After the hooks, the core restores the hop-by-hop fields, removes generated framing fields, and fails with 500 for an invalid header name or value.
- `TestOutboundTransportFailureClosesBodyWithoutDelegating` is a white-box test. Through the gateway, `ReverseProxy` gives the transport a body wrapper whose `Close` has no visible effect, so only a direct call of the private transport wrapper shows that it closes the body.
- `TestTypedStateIsolationAndNilValues`. Old: the test made a `RequestContext` itself. New: the zero value is not useful, so the test gets the gateway context through the gateway. After completion, the core drops the state.
- `TestConcurrentRequestStateIsolation`. Old: an `HTTPError` made the local response. New: a routing miss makes it.
- `TestRequestTagExtension`. Slice 1 removed the `requesttag` extension. The new test uses an extension that is defined in the test.
- Selector failures (slice-1 test `gateway_test.go:TestCustomUpstreamSelector`). Old: 502. New: 500 (ADR-0017).
- Outbound header check (slice-1 behavior, `hooks_test.go:TestOutboundHeaderCheckDoesNotDependOnHooks`). Old: a header that middleware set went to the transport without a check, so the transport forwarded a name such as `X_User` and failed the round trip (502) for a control character in a value. New: the core checks the outbound headers and trailer names for each request, also without hooks, and a failure is 500 in `PhaseBeforeUpstream` (ADR-0019). Traffic with such headers from middleware now fails.
- Upstream hosts (slice-1 tests `gateway_test.go:TestStaticUpstreamAcceptsValidURL` and `gateway_test.go:TestStaticUpstreamRejectsInvalidURL`, extended). Old: any host. New: only a host that the transport sends without changes, so `OutboundRequest.Host` is the wire value; an internationalized name needs its Punycode form, and IPv6 zones are rejected (ADR-0019).

New tests without a prototype row: `lifecycle_test.go:TestSecureDefaultRejectionReachesCompletion`, `lifecycle_test.go:TestStateIsDroppedAfterCompletion`, `lifecycle_test.go:TestMissingGatewayContextFailsClosed`, `outbound_test.go:TestBeforeUpstreamHeaderRules`, `outbound_test.go:TestBeforeUpstreamInvalidHeaderFails`, `response_test.go:TestResponseHeaderRulesAfterHooks`, `response_test.go:TestResponseContentLengthAfterHooks`, `response_test.go:TestUpgradeResponseHeaderRulesAfterHooks`, `response_test.go:TestUpgradeProtocolIsRestoredAfterHooks`, `response_test.go:TestUpgradeMismatchFromUpstream`, `response_test.go:TestResponseHooksSkipInformationalResponses`, `response_test.go:TestConnectionTokensDoNotDependOnMapOrder`, `response_test.go:TestUpgradeConnectionTokensDoNotDependOnMapOrder`, `response_test.go:TestResponseHooksGetTheGatewayContext`, `response_test.go:TestResponseHookCannotCrossTheUpgradeBoundary`, `outbound_test.go:TestOutboundHostIsTheValueThatTheUpstreamGets`, `outbound_test.go:TestSelectedHostThatTheTransportChangesIsRejected`, `inbound_test.go:TestDeclaredManagedHeadersAreRemoved`, `gateway_test.go:TestHookProvidersAccepted`, `example_test.go:ExampleBeforeUpstreamHook`, `example_test.go:ExampleSetState`, `hooks_test.go:TestHookAdaptersExposeOnlyTheirPhase`, `hooks_test.go:TestOutboundHeaderCheckDoesNotDependOnHooks`, `outbound_internal_test.go:TestOutboundCheckNamesTheFailedCheck`, `fields_internal_test.go:TestRemovedFieldSetsFollowReservedHeaders`, and `benchmark_test.go:BenchmarkServeHTTPHooks`.

## Slice 3: rejections, renderer, and typed failures

| Prototype test | Behavior | Status | New tests | ADR |
| --- | --- | --- | --- | --- |
| `diagnostics_test.go:TestProxyStreamDiagnosticOmitsDetailsAndPreservesAbort` | A stream failure diagnostic has no details and does not change the abort or completion | pending | | 0009 |
| `diagnostics_test.go:TestProxyErrorRemainsAvailableWithoutAutomaticLogging` | The original proxy error stays available to the caller and is not logged automatically | pending | | 0009, 0013 |
| `lifecycle_test.go:TestHookErrorsStopPhaseAndHideInternalDetails` _(split)_ | A typed rejection from a hook sets the status and the public message; a rejection with a status that is not 4xx or 5xx gives a generic 500 | pending | | 0013 |
| `lifecycle_test.go:TestCompletionOnEarlyExits` _(split)_ | A typed rejection from a request hook gives its status (429) and is visible at completion | pending | | 0013 |

## Slice 4: hardened server

| Prototype test | Behavior | Status | New tests | ADR |
| --- | --- | --- | --- | --- |
| `cmd/gateway/main_test.go:TestTLSHandshakeTimeout` | A stalled TLS handshake times out and the listener exits on shutdown | pending | | 0016 |
| `cmd/gateway/main_test.go:TestServerDiagnosticsDoNotChangeCompletionOrEchoDetails` | Server diagnostics do not change timeouts, panics, responses, or completion, and contain no details | pending | | 0016 |
| `cmd/gateway/main_test.go:TestFailureLogsDoNotEchoConfigurationOrErrors` | Startup and shutdown failure logs give a classification only | pending | | |
| `cmd/gateway/resource_limits_test.go:TestInboundHeaderLimitRejectsBeforeGatewayEntry` | Oversized inbound headers are rejected before gateway entry and completion | pending | | 0016 |

## Slice 5: core protections

| Prototype test | Behavior | Status | New tests | ADR |
| --- | --- | --- | --- | --- |
| `extensions/bodylimit/bodylimit_test.go:TestGatewayBodyLimits` | Request bodies over the limit are rejected; bodies within it pass | pending | | 0012 |
| `extensions/bodylimit/bodylimit_test.go:TestMiddlewareDoesNotReadAheadOrWrapResponseWriter` | The body limit does not read ahead or wrap the response writer | pending | | 0012 |
| `extensions/bodylimit/bodylimit_test.go:TestNegativeLimitPanics` | A negative body limit is a configuration error | pending | | 0007 |
| `extensions/admission/admission_test.go:TestPositiveLimitRequired` | The admission limit must be positive | pending | | 0007 |
| `extensions/admission/admission_test.go:TestNoQueueAndNoBodyReadOnSaturation` | At saturation, requests are rejected at once, without a queue and without reading the body | pending | | 0012 |
| `extensions/admission/admission_test.go:TestRequestAndResponseWriterArePassedThrough` | Admitted requests get the original request and response writer | pending | | |
| `extensions/admission/admission_test.go:TestPanicUnwindsAndReleasesSlot` | A panic releases the admission slot and continues | pending | | |
| `extensions/admission/admission_test.go:TestCancellationHoldsSlotUntilDownstreamReturns` | A canceled request keeps its slot until downstream handling returns | pending | | |
| `extensions/admission/admission_test.go:TestActiveCallsStayWithinLimit` | Concurrent active requests never exceed the limit | pending | | |
| `extensions/admission/integration_test.go:TestGatewaySaturationIsLocalAndCompletionRunsAfterUnwind` | A saturation rejection is local; completion runs after middleware unwinds | pending | | 0009 |
| `extensions/admission/integration_test.go:TestHTTP2StreamsShareBudgetAndKeepSlotsWhileStreaming` | HTTP/2 streams share the budget and keep their slots while they stream | pending | | |
| `extensions/admission/integration_test.go:TestCancellationDuringPoolAcquisitionReleasesAdmissionSlot` | Cancellation while waiting for a pooled connection releases the slot | pending | | |
| `proxy_safety_test.go:TestRequestBodyStreamsBeforeClientEOF` _(split)_ | A slow streaming upload keeps its admission slot until it ends; another request gets 503 | pending | | 0012 |
| `extensions/egress/egress_test.go:TestInvalidConfiguration` | Construction rejects invalid egress configuration | pending | | 0007 |
| `extensions/egress/egress_test.go:TestOriginNormalization` | Allowed origins are normalized before comparison | pending | | |
| `extensions/egress/egress_test.go:TestDeniedRequestClosesBodyWithoutDelegation` | A denied request closes its body and is not sent | pending | | |
| `extensions/egress/egress_test.go:TestAddressClassification` | Addresses are classified as allowed or denied, including special ranges | pending | | 0012 |
| `extensions/egress/egress_test.go:TestLookupAnswersAreValidatedBeforeAnyDial` | All DNS answers are validated before any dial | pending | | 0012 |
| `extensions/egress/egress_test.go:TestDialUsesValidatedLiteralAndPreservesPort` | The dial uses the validated IP literal and keeps the port | pending | | |
| `extensions/egress/egress_test.go:TestResolutionChangesCannotReuseHostnameAtDial` | A changed DNS answer cannot bypass validation at dial time | pending | | 0012 |
| `extensions/egress/egress_test.go:TestDialFallbackUsesOnlyValidatedAddressesAndOneBudget` | Dial fallback uses only validated addresses and one time budget | pending | | |
| `extensions/egress/egress_test.go:TestCancellationAndConnectionBudget` | Cancellation stops a dial and the connection budget is enforced | pending | | |
| `extensions/egress/egress_test.go:TestLookupErrorDoesNotDial` | A lookup error does not cause a dial | pending | | |
| `extensions/egress/egress_test.go:TestUnconfiguredDialAuthorityIsDenied` | A dial to an authority that is not configured is denied | pending | | |
| `extensions/egress/egress_test.go:TestConfigurationIsCopied` | Later changes to the caller's configuration values have no effect | pending | | |
| `extensions/egress/egress_test.go:TestConcurrentAddressChecks` | Address checks are safe for concurrent use | pending | | |
| `extensions/egress/integration_test.go:TestGatewayPreservesHTTPAndTLSIdentityAndReusesConnections` | Egress control keeps the Host, TLS server name, and connection reuse | pending | | |
| `extensions/egress/integration_test.go:TestConnectionBudgetDoesNotLimitResponseStreaming` | The connection budget does not limit response streaming | pending | | |
| `extensions/egress/integration_test.go:TestTLSCertificateVerificationIsNotDisabled` | TLS certificate verification stays on | pending | | 0012 |
| `extensions/egress/integration_test.go:TestGatewayDenialRemainsAnExecutionErrorAfterMiddlewareUnwinds` | An egress denial is an execution fact at completion | pending | | 0009 |
| `extensions/egress/integration_test.go:TestExistingPoolIsNotSharedAndSettingsArePreserved` | Egress control does not share an existing pool and keeps its settings | pending | | |

## Slice 6: bundled extensions

| Prototype test | Behavior | Status | New tests | ADR |
| --- | --- | --- | --- | --- |
| `extensions/accesslog/accesslog_test.go:TestRecordJoinsHTTPGatewayAndTraceObservations` | A record joins HTTP observations, execution facts, and trace IDs | pending | | 0009 |
| `extensions/accesslog/accesslog_test.go:TestLocalResponsesAndGatewayFailures` | Local responses and gateway failures are recorded correctly | pending | | 0009 |
| `extensions/accesslog/accesslog_test.go:TestPanicAccountingDoesNotInventSuccessfulStatus` | A panic is not recorded with a successful status | pending | | 0009 |
| `extensions/accesslog/accesslog_test.go:TestStreamingReadFailureKeepsPartialHTTPObservations` | A streaming read failure keeps the partial byte count and status | pending | | 0009 |
| `extensions/accesslog/accesslog_test.go:TestPartialWriteErrorIsSeparateFromGatewayError` | A client write error is recorded separately from a gateway error | pending | | 0009 |
| `extensions/accesslog/accesslog_test.go:TestStreamingResponseFlushesBeforeCompletion` | Instrumentation does not stop flushes before completion | pending | | |
| `extensions/accesslog/accesslog_test.go:TestProtocolUpgradePreservesHijacking` | Instrumentation keeps hijacking for protocol upgrades | pending | | |
| `extensions/accesslog/accesslog_test.go:TestConcurrentRecordsKeepRequestAndTraceIDsIsolated` | Concurrent records keep their own request and trace IDs | pending | | |
| `extensions/accesslog/accesslog_test.go:TestMissingMiddlewareAndStandaloneUse` | The extension works without its middleware and outside a gateway | pending | | |
| `extensions/accesslog/response_test.go:TestResponseWriterCapabilityCombinations` | The wrapped response writer keeps each optional interface of the original | pending | | |
| `extensions/accesslog/response_test.go:TestControllerHijackThroughUnwrapRemainsObservable` | A hijack through `http.ResponseController` is still observed | pending | | |
| `extensions/accesslog/response_test.go:TestResponseStatusesAndRequestID` | Statuses, including informational ones, and the request ID are recorded | pending | | |
| `extensions/accesslog/response_test.go:TestResponseControllerOperationsAndFlushError` | `http.ResponseController` operations work and flush errors are observed | pending | | |
| `cmd/gateway/main_test.go:TestCompletionLogUsesOnlyApprovedFields` | The completion log contains only approved fields | pending | | |
| `cmd/gateway/main_test.go:TestCompletionLogValueAllowlists` | Completion log values come from allowlists | pending | | |
| `extensions/outboundsign/outboundsign_test.go:TestSignsRewrittenDestinationAndEscapedURIWithoutChangingInboundRequest` | The signature covers the rewritten destination and escaped URI; the inbound request does not change | pending | | 0003 |
| `extensions/outboundsign/outboundsign_test.go:TestSigningFailurePreventsForwardingAndCompletesAfterUnwind` | A signing failure stops forwarding; completion runs after unwind | pending | | 0009 |
| `extensions/outboundsign/outboundsign_test.go:TestConcurrentRoutesUseTheirOwnSigningKeys` | Concurrent routes use their own signing keys | pending | | |
