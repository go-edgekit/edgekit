# Rewrite ledger

**Status:** Working document for the core rewrite. Delete it, together with the rewrite line in [AGENTS.md](../AGENTS.md), when every row is covered or dropped.

The prototype is kept at the git tag `prototype-final`. Its tests are the behavior specification for the rewrite. The rewrite does not port them. It writes new tests against the new API that examine the same conditions.

## Rules

- Each prototype test has one row. Remove a prototype test only in the same change that sets its row to _covered_ or _dropped_.
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
| `gateway_test.go:TestProxyForwardsRequestAndAppliesRouteMiddleware` | A matched route forwards the request upstream and runs its route middleware | pending | | |
| `gateway_test.go:TestGlobalAndRouteMiddlewareOrder` | Global middleware wraps route middleware in registration order | pending | | 0004 |
| `gateway_test.go:TestInvalidRoute` | Construction rejects invalid route configuration | pending | | 0007 |
| `proxy_safety_test.go:TestForwardingTrustAndHopByHopHeaders` | Inbound forwarding headers are not trusted; hop-by-hop headers are removed | pending | | 0012 |
| `proxy_safety_test.go:TestTransportReusesConnectionsAcrossRoutes` | Routes to the same upstream share one connection pool | pending | | |
| `proxy_safety_test.go:TestResponseHeaderTimeoutCancelsUpstream` | The response-header timeout cancels the upstream request | pending | | 0012 |
| `proxy_safety_test.go:TestCancellationWhileWaitingForConnectionPool` | Client cancellation ends a wait for a pooled connection | pending | | |
| `proxy_safety_test.go:TestRequestBodyStreamsBeforeClientEOF` | The request body streams upstream before the client finishes sending | pending | | |
| `cmd/gateway/main_test.go:TestUpstreamTransportPolicy` | The upstream transport has bounded timeouts and pool settings | pending | | 0012 |
| `cmd/gateway/main_test.go:TestHeaderTimeoutDoesNotLimitResponseStream` | The response-header timeout does not cut a streaming response body | pending | | 0012 |
| `cmd/gateway/main_test.go:TestUpstreamDialRespectsCanceledContext` | The upstream dial stops when the request context is canceled, also before a connection exists | pending | | |

## Slice 2: lifecycle hooks and request state

| Prototype test | Behavior | Status | New tests | ADR |
| --- | --- | --- | --- | --- |
| `lifecycle_test.go:TestLifecycleContextAndOrder` | Hooks and middleware share one gateway context; phases run in a fixed order | pending | | 0003, 0005 |
| `lifecycle_test.go:TestCompletionOnEarlyExits` | Completion runs when middleware or a hook ends handling early | pending | | 0009 |
| `lifecycle_test.go:TestHookErrorsStopPhaseAndHideInternalDetails` | A hook error stops its phase; the response contains no internal details | pending | | 0009, 0013 |
| `lifecycle_test.go:TestTransportErrorReachesCompletion` | A transport error is an execution fact visible at completion | pending | | 0009 |
| `lifecycle_test.go:TestCompletionDuringPanicUnwinding` | Completion runs while a panic unwinds, and the panic continues | pending | | 0009 |
| `lifecycle_test.go:TestStreamingFailureAbortsAndCompletes` | A failure during response streaming aborts the response and completion runs | pending | | 0009 |
| `lifecycle_test.go:TestRequestTagExtension` | Middleware and hooks of one extension share typed request state | pending | | 0005 |
| `lifecycle_test.go:TestTypedStateIsolationAndNilValues` | Typed state keys are isolated from each other; nil values are stored and read correctly | pending | | 0005 |
| `lifecycle_test.go:TestConcurrentRequestStateIsolation` | Concurrent requests do not see each other's state | pending | | 0005 |
| `lifecycle_test.go:TestCancellationReachesHooks` | Client cancellation is visible to hooks | pending | | 0005 |
| `lifecycle_test.go:TestCompletionOnOutboundPanic` | Completion runs when an outbound hook panics | pending | | 0009 |
| `lifecycle_test.go:TestInvalidExtensions` | Construction rejects invalid hook providers | pending | | 0007 |
| `outbound_test.go:TestOutboundTransportClonesMetadataAndPassesTheHookRequest` | Outbound hooks get a copy of request metadata; the transport gets the request that the hooks made; the body streams | pending | | 0003 |
| `outbound_test.go:TestOutboundTransportFailureClosesBodyWithoutDelegating` | An outbound hook failure stops later hooks, closes the body, and does not call the transport | pending | | 0003 |
| `proxy_safety_test.go:TestConfiguredTransportWithAndWithoutOutboundHooks` | A configured transport is used with and without outbound hooks | pending | | 0003 |
| `proxy_safety_test.go:TestClientCancellationReachesUpstreamAndCompletion` | Client cancellation reaches the upstream and is visible at completion | pending | | 0009 |
| `cmd/gateway/resource_limits_test.go:TestUpstreamHeaderLimitBeforeResponseHooks` | Oversized upstream response headers fail before response hooks run, on HTTP/1.1 and HTTP/2 | pending | | 0012 |

## Slice 3: rejections, renderer, and typed failures

| Prototype test | Behavior | Status | New tests | ADR |
| --- | --- | --- | --- | --- |
| `diagnostics_test.go:TestProxyStreamDiagnosticOmitsDetailsAndPreservesAbort` | A stream failure diagnostic has no details and does not change the abort or completion | pending | | 0009 |
| `diagnostics_test.go:TestProxyErrorRemainsAvailableWithoutAutomaticLogging` | The original proxy error stays available to the caller and is not logged automatically | pending | | 0009, 0013 |
| `diagnostics_test.go:TestGatewayConstructionDoesNotReconfigureProcessLogging` | Gateway construction does not change process-global logging | pending | | |

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
