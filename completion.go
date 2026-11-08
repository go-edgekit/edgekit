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

	// RejectionReason is the reason of an explicit rejection: by the core,
	// or by a hook with a valid [Rejection]. It is empty when no explicit
	// rejection was recorded. Responses that middleware writes do not set it.
	// A rejection is not a failure: Failure is then nil.
	RejectionReason RejectionReason

	// Failure is the first failure that the gateway observed, or nil.
	Failure *Failure

	// ConnectionObtained is true when the transport reported, with
	// [net/http/httptrace.ClientTrace.GotConn], a connection for at least one
	// attempt of the round trip. The connection can be to a proxy that the
	// transport uses.
	ConnectionObtained bool

	// RequestWritten is true when the transport reported, with
	// [net/http/httptrace.ClientTrace.WroteRequest], that it wrote the
	// complete request in at least one attempt. It does not prove that the
	// upstream read the request.
	//
	// ConnectionObtained and RequestWritten are facts about the request, not
	// about the attempt that failed, because the transport can retry. False
	// means that the transport did not report the event before completion
	// started. [net/http.Transport] reports both events; another transport
	// reports them only when it calls the same trace hooks.
	RequestWritten bool

	// Aborted is true when the gateway-managed handler chain did not return
	// normally, such as during a panic. A response-body read failure has
	// PhaseResponseBody. A response-write failure can abort without a Failure.
	Aborted bool
}

// Failure is a failure that gateway execution observed.
type Failure struct {
	// Phase is where the failure occurred.
	Phase Phase

	// Err is the error, without changes. It can contain secrets or request
	// data, so do not log it or send it to clients without application policy.
	// It wraps [ErrInvalidRejection] when a hook returned a [Rejection] that
	// is not valid.
	Err error
}

// Phase identifies where a [Failure] occurred.
type Phase int

// The phases of a [Failure]. A phase is a stage of request handling, not a
// hook: a stage can fail also when no hook is configured for it. The
// constants are in execution order. Their numeric values are not stable
// between versions, so logs and stored records must use [Phase.String].
const (
	// PhaseUnknown is a failure that [net/http/httputil.ReverseProxy]
	// reports before a known stage of the gateway.
	PhaseUnknown Phase = iota

	// PhaseRouteMatched is the stage of the [RouteMatchedHook] checks.
	PhaseRouteMatched

	// PhaseRequestBuffering is complete inbound buffering before middleware.
	// Only established inbound input or deadline failures give 400 or 408;
	// unknown reader errors and internal defects give 500.
	PhaseRequestBuffering

	// PhaseUpstreamSelection is the selection of the upstream URL.
	PhaseUpstreamSelection

	// PhaseBeforeUpstream is the stage after the route middleware and before
	// the transport: the [BeforeUpstreamHook] hooks and the checks of the
	// outbound request (see [Gateway]). The checks also run when no hook is
	// configured, so this phase can record an invalid header or Upgrade
	// value that middleware set.
	PhaseBeforeUpstream

	// PhaseRoundTrip is the round trip of the transport to the upstream.
	PhaseRoundTrip

	// PhaseUpstreamResponse is the stage of the [UpstreamResponseHook]
	// hooks.
	PhaseUpstreamResponse

	// PhaseProtocolSwitch is the switch to another protocol after the
	// gateway accepted a 101 Switching Protocols response: the check of the
	// protocol, the hijack of the client connection, and the write of the 101
	// response to it.
	PhaseProtocolSwitch

	// PhaseResponseBody is the read of the final response body after the
	// UpstreamResponse hooks. A read error other than io.EOF is recorded
	// without changes, also after the response is committed. This phase does
	// not cover response-write errors or upgraded connections.
	PhaseResponseBody
)

// String returns a fixed name for the phase, for logs.
func (p Phase) String() string {
	switch p {
	case PhaseUnknown:
		return "unknown"
	case PhaseRouteMatched:
		return "route_matched"
	case PhaseRequestBuffering:
		return "request_buffering"
	case PhaseUpstreamSelection:
		return "upstream_selection"
	case PhaseBeforeUpstream:
		return "before_upstream"
	case PhaseRoundTrip:
		return "round_trip"
	case PhaseUpstreamResponse:
		return "upstream_response"
	case PhaseProtocolSwitch:
		return "protocol_switch"
	case PhaseResponseBody:
		return "response_body"
	default:
		return "invalid"
	}
}

// RejectionReason is a public code that names the check that refused a
// request, such as "invalid_path". It is a static name, never an identifier
// or a value from the request.
//
// The codes of the core are the constants of this package and never contain
// ".". The codes of extensions have the form "<extension>.<check>" (see
// [Rejection]), so the two sets cannot collide.
type RejectionReason string

// The rejection reasons of the secure-default checks (see [Gateway]).
const (
	ReasonInvalidMethod        RejectionReason = "invalid_method"
	ReasonInvalidRequestTarget RejectionReason = "invalid_request_target"
	ReasonInvalidPath          RejectionReason = "invalid_path"
	ReasonInvalidQuery         RejectionReason = "invalid_query"
	ReasonInvalidHeaderName    RejectionReason = "invalid_header_name"
	ReasonInvalidUpgrade       RejectionReason = "invalid_upgrade"
)

// ReasonNoRoute is the rejection reason of a request that matched no route.
const ReasonNoRoute RejectionReason = "no_route"

// The rejection reasons of the core protections (ADR-0025).
const (
	// ReasonBodyTooLarge is the reason of a 413 rejection after matching:
	// the declared length is more than the fixed effective body limit. An
	// overflow while the body streams is a [Failure] instead (see
	// [ErrRequestBodyTooLarge]).
	ReasonBodyTooLarge RejectionReason = "body_too_large"

	// ReasonAdmissionSaturated is the reason of a 503 rejection when an
	// [AdmissionBudget] has no free slot.
	ReasonAdmissionSaturated RejectionReason = "admission_saturated"

	// ReasonRequestBufferSaturated is a 503 rejection when the shared byte
	// budget cannot reserve the route's effective body limit.
	ReasonRequestBufferSaturated RejectionReason = "request_buffer_saturated"
)

// completion reads the transport facts once, so all completion hooks get the
// same snapshot.
func (rc *RequestContext) completion(duration time.Duration, aborted bool) Completion {
	routePrefix, _ := rc.RoutePrefix()
	c := Completion{
		Start:            rc.start,
		Duration:         duration,
		RoutePrefix:      routePrefix,
		SelectedUpstream: rc.upstream,
		RejectionReason:  rc.rejection,
		Failure:          rc.failure,
		Aborted:          aborted,
	}

	if rc.facts != nil {
		c.ConnectionObtained = rc.facts.connected.Load()
		c.RequestWritten = rc.facts.written.Load()
	}

	return c
}

// copyCompletion gives each hook its own copy of the pointer fields, so a hook
// that changes its copy does not change the copy of another hook.
func copyCompletion(c Completion) Completion {
	if c.SelectedUpstream != nil {
		upstream := *c.SelectedUpstream
		c.SelectedUpstream = &upstream
	}

	if c.Failure != nil {
		failure := *c.Failure
		c.Failure = &failure
	}

	return c
}
