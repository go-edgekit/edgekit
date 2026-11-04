package edgekit

import (
	"net/url"
	"time"
)

// Completion holds the execution facts of one request, which the gateway
// gives to [RequestCompletedHook] values. It is a snapshot: each hook gets its
// own copy, and a hook can keep it after completion.
//
// Completion reports what gateway execution established, not what the client
// received. It contains no HTTP status and no byte counts: response-observing
// middleware owns those observations. A field that is empty means that the
// fact was not established, not that the opposite is true.
type Completion struct {
	// Start is the time when the request entered the gateway.
	Start time.Time

	// Duration is the time from Start to the start of completion.
	Duration time.Duration

	// RoutePrefix is the prefix of the matched route, or "" when no route
	// matched.
	RoutePrefix string

	// SelectedUpstream is a copy of the URL that the upstream selector
	// returned, or nil when no upstream was selected. A selected upstream does
	// not prove that the gateway connected to it.
	SelectedUpstream *url.URL

	// RejectionReason is the reason when the core rejected the request. It is
	// empty when the core recorded no rejection. Rejections by middleware or
	// hooks do not set it.
	RejectionReason RejectionReason

	// Failure is the first failure that the gateway observed, or nil.
	Failure *Failure

	// Aborted is true when the gateway-managed handler chain did not return
	// normally, such as during a panic. A failure while the response streams
	// to the client can abort without a Failure.
	Aborted bool
}

// Failure is a failure that gateway execution observed.
type Failure struct {
	// Phase is where the failure occurred.
	Phase Phase

	// Err is the error, without changes. It can contain secrets or request
	// data, so do not log it or send it to clients without application policy.
	Err error
}

// Phase identifies where a [Failure] occurred.
type Phase int

// The phases of a [Failure]. A phase is a stage of request handling, not a
// hook: a stage can fail also when no hook is configured for it.
const (
	// PhaseUnknown is a failure that [net/http/httputil.ReverseProxy]
	// reports without a known source, such as a protocol switch that the
	// upstream did not accept.
	PhaseUnknown Phase = iota

	// PhaseRequestReceived is the stage of the [RequestReceivedHook] hooks.
	PhaseRequestReceived

	// PhaseUpstreamSelection is the selection of the upstream URL.
	PhaseUpstreamSelection

	// PhaseBeforeUpstream is the stage of the [BeforeUpstreamHook] hooks and
	// the outbound header check (see [Gateway]), after the route middleware
	// and before the transport. It also records an invalid header that
	// middleware set when no hook is configured.
	PhaseBeforeUpstream

	// PhaseRoundTrip is the round trip of the transport to the upstream.
	PhaseRoundTrip

	// PhaseUpstreamResponse is the stage of the [UpstreamResponseHook]
	// hooks.
	PhaseUpstreamResponse
)

// String returns a fixed name for the phase, for logs.
func (p Phase) String() string {
	switch p {
	case PhaseUnknown:
		return "unknown"
	case PhaseRequestReceived:
		return "request_received"
	case PhaseUpstreamSelection:
		return "upstream_selection"
	case PhaseBeforeUpstream:
		return "before_upstream"
	case PhaseRoundTrip:
		return "round_trip"
	case PhaseUpstreamResponse:
		return "upstream_response"
	default:
		return "invalid"
	}
}

// RejectionReason is a public code for a rejection by the core, such as
// "invalid_path". The core sets it only from the constants of this package. It
// names the check that refused the request, never the value that failed the
// check, so it contains no request content.
type RejectionReason string

// The rejection reasons of the secure-default checks (see [Gateway]).
const (
	ReasonInvalidMethod        RejectionReason = "invalid_method"
	ReasonInvalidRequestTarget RejectionReason = "invalid_request_target"
	ReasonInvalidPath          RejectionReason = "invalid_path"
	ReasonInvalidQuery         RejectionReason = "invalid_query"
	ReasonInvalidHeaderName    RejectionReason = "invalid_header_name"
)

// ReasonNoRoute is the rejection reason of a request that matched no route.
const ReasonNoRoute RejectionReason = "no_route"

// completion returns a new snapshot for each hook, so a hook that changes its
// copy does not change the copy of another hook.
func (rc *RequestContext) completion(duration time.Duration, aborted bool) Completion {
	c := Completion{
		Start:           rc.start,
		Duration:        duration,
		RoutePrefix:     rc.route,
		RejectionReason: rc.rejection,
		Aborted:         aborted,
	}

	if rc.upstream != nil {
		upstream := *rc.upstream
		c.SelectedUpstream = &upstream
	}

	if rc.failure != nil {
		failure := *rc.failure
		c.Failure = &failure
	}

	return c
}
