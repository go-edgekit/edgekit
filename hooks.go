package edgekit

import (
	"context"
	"errors"
	"net/http"
	"slices"
)

// Hooks holds the lifecycle hooks of a gateway, one list for each phase. Use
// it in [Config.Hooks] to set the execution order of each phase
// independently. The zero value has no hooks.
//
// Each list runs serially in its written order. A value in more than one list
// runs in each of those phases. Repeated entries run once per occurrence. A
// rejection or error stops the later hooks of that processing phase; completion
// still runs as described by [RequestCompletedHook].
//
// [New] copies the lists, but shares their values between concurrent requests.
// Each value must be safe for concurrent use. Keep request state in
// [RequestContext], not in the hook. [New] rejects nil entries, including typed
// nil entries, with [ErrInvalidConfiguration]. Nil and empty lists are valid.
// Registration does not declare managed headers; use [Config.HeaderOwners].
// Hooks are trusted code and must obey the contract of each phase.
type Hooks struct {
	// RouteMatched lists the route-aware checks that run after the declared
	// body-length check and admission, before buffering and middleware.
	RouteMatched []RouteMatchedHook

	// BeforeUpstream lists the hooks that run after the outbound rewrite and
	// before the transport.
	BeforeUpstream []BeforeUpstreamHook

	// UpstreamResponse lists the hooks that run before the final upstream
	// response is sent.
	UpstreamResponse []UpstreamResponseHook

	// RequestCompleted lists the hooks that run after gateway-managed handling
	// ends.
	RequestCompleted []RequestCompletedHook
}

// RouteMatchedHook is a route-aware check before request-body processing.
// Register it in [Hooks.RouteMatched]. It runs only after successful route
// matching, the declared body-length check, and gateway and route admission.
// A match alone does not guarantee invocation. Buffering and all processing
// middleware run later, so their contexts and state are not available yet.
//
// ctx is the entry execution context and r is the sanitized inbound request.
// The hook must not change r or read its body. It can set request state and
// return a rejection or failure under the rules of [Rejection]. The gateway
// shares hook values between concurrent requests; keep request state in rc.
// Do not retain r or rc after handling.
//
// A rejection or error stops later hooks and processing. Completion still
// runs. An error gives 500 without details, or 499 for an established client
// cancellation, and a [Failure] in [PhaseRouteMatched].
type RouteMatchedHook interface {
	RouteMatched(ctx context.Context, rc *RequestContext, r *http.Request) (*Rejection, error)
}

// RouteMatchedHookFunc adapts a function to [RouteMatchedHook]. The function
// must obey that interface's concurrency, ownership, and failure contract.
// [New] rejects a nil function.
type RouteMatchedHookFunc func(ctx context.Context, rc *RequestContext, r *http.Request) (*Rejection, error)

// RouteMatched calls f(ctx, rc, r).
func (f RouteMatchedHookFunc) RouteMatched(ctx context.Context, rc *RequestContext, r *http.Request) (*Rejection, error) {
	return f(ctx, rc, r)
}

// BeforeUpstreamHook is a lifecycle hook that runs after the route middleware,
// the upstream selection, and the rewrite of the outbound request, before the
// transport sends it. Register it in [Hooks.BeforeUpstream]. Use it, for example, to
// sign the outbound request.
//
// out gives the rewritten outbound request. The hook can change only its
// header map, so the hook cannot change the destination. The gateway then
// applies the rules in the [Gateway] documentation to the header map. The hook
// runs once for each request, not once for each connection attempt of the
// transport. In [Hooks.BeforeUpstream], register a hook that signs header
// fields after the hooks that change those fields.
//
// The result of BeforeUpstream follows the rules of [Rejection]. After a
// rejection or an error, the later hooks of the phase do not run, and the
// transport is not called. For an error, the gateway sends 500 Internal Server
// Error without error details, or 499 when the error wraps [context.Canceled]
// and the client is gone, and completion gets the error as a [Failure] in
// [PhaseBeforeUpstream].
type BeforeUpstreamHook interface {
	BeforeUpstream(ctx context.Context, rc *RequestContext, out *OutboundRequest) (*Rejection, error)
}

// BeforeUpstreamHookFunc is an adapter to use an ordinary function as a
// [BeforeUpstreamHook], as [http.HandlerFunc] is for [http.Handler]. The
// function must obey the contract of [BeforeUpstreamHook]. [New] rejects a nil
// BeforeUpstreamHookFunc.
type BeforeUpstreamHookFunc func(ctx context.Context, rc *RequestContext, out *OutboundRequest) (*Rejection, error)

// BeforeUpstream calls f(ctx, rc, out).
func (f BeforeUpstreamHookFunc) BeforeUpstream(ctx context.Context, rc *RequestContext, out *OutboundRequest) (*Rejection, error) {
	return f(ctx, rc, out)
}

// UpstreamResponseHook is a lifecycle hook that runs after the upstream
// response headers arrive and before the gateway sends the final headers to
// the client. Register it in [Hooks.UpstreamResponse]. Use it, for example, to add
// response headers.
//
// The hook does not run for responses that the gateway or middleware make, or
// for informational (1xx) responses, except 101 Switching Protocols. The
// hook can change resp. A hook that replaces resp.Body must close the original
// body and update or delete Content-Length. A nil resp.Body is an empty body.
// After the hooks, the gateway applies the rules in the [Gateway]
// documentation to the response headers.
//
// The hook can change a final status only to another status from 200 to 599.
// It must not change the status of a 101 response, and it must not change
// another status to 101, because the gateway chose the handling of a protocol
// switch before the hooks. Otherwise the gateway closes the upstream body,
// sends 500 Internal Server Error, and completion gets a [Failure] in
// [PhaseUpstreamResponse]. Changes to the Connection and Upgrade fields of a
// 101 response have no effect: the gateway restores the protocol switch of
// the upstream.
//
// resp.Request is the outbound request that the gateway sent, also when the
// transport returned another request.
//
// The result of UpstreamResponse follows the rules of [Rejection]. After a
// rejection or an error, the later hooks of the phase do not run, and the
// gateway closes the upstream body. For an error, the gateway sends 500
// Internal Server Error without error details, 502 Bad Gateway when the error
// wraps [ErrInvalidUpstreamResponse], or 499 when the error wraps
// [context.Canceled] and the client is gone. Completion gets the error as a
// [Failure] in [PhaseUpstreamResponse].
//
// A hook must not replace the body of a 101 response with a body that does
// not implement [io.ReadWriteCloser]. Otherwise the gateway sends 500
// Internal Server Error.
type UpstreamResponseHook interface {
	UpstreamResponse(ctx context.Context, rc *RequestContext, resp *http.Response) (*Rejection, error)
}

// UpstreamResponseHookFunc is an adapter to use an ordinary function as an
// [UpstreamResponseHook], as [http.HandlerFunc] is for [http.Handler]. The
// function must obey the contract of [UpstreamResponseHook]. [New] rejects a
// nil UpstreamResponseHookFunc.
type UpstreamResponseHookFunc func(ctx context.Context, rc *RequestContext, resp *http.Response) (*Rejection, error)

// UpstreamResponse calls f(ctx, rc, resp).
func (f UpstreamResponseHookFunc) UpstreamResponse(ctx context.Context, rc *RequestContext, resp *http.Response) (*Rejection, error) {
	return f(ctx, rc, resp)
}

// ErrInvalidUpstreamResponse is the error that an [UpstreamResponseHook] wraps
// when it finds that the upstream response is not valid, such as a missing
// required signature. The gateway then sends 502 Bad Gateway, because the
// failure is in the upstream. In another phase, it has no effect on the
// status. Use [errors.Is] to identify it.
var ErrInvalidUpstreamResponse = errors.New("invalid upstream response")

// RequestCompletedHook is a lifecycle hook that runs once for each request,
// at completion: after the gateway-managed handler chain returned or finished
// unwinding, also during a panic, and after a rejection by the secure
// defaults. Register it in [Hooks.RequestCompleted].
//
// ctx is the context that the gateway got with the request, together with the
// gateway context. It is not a context that middleware derived, and it can be
// canceled already. Completion does not mean that the client received the
// response.
//
// The hook observes only: it must not panic, and it cannot write a response.
// It must not keep rc after it returns. It can keep c.
type RequestCompletedHook interface {
	RequestCompleted(ctx context.Context, rc *RequestContext, c Completion)
}

// RequestCompletedHookFunc is an adapter to use an ordinary function as a
// [RequestCompletedHook], as [http.HandlerFunc] is for [http.Handler]. The
// function must obey the contract of [RequestCompletedHook]. [New] rejects a
// nil RequestCompletedHookFunc.
type RequestCompletedHookFunc func(ctx context.Context, rc *RequestContext, c Completion)

// RequestCompleted calls f(ctx, rc, c).
func (f RequestCompletedHookFunc) RequestCompleted(ctx context.Context, rc *RequestContext, c Completion) {
	f(ctx, rc, c)
}

// HeaderOwner declares the managed headers that an extension owns, such as a
// header that tells the upstream the authenticated caller. Register it in
// [Config.HeaderOwners], independently of lifecycle hooks.
//
// The gateway removes these headers from inbound requests together with the
// headers in [Config.ManagedHeaders], so only middleware and hooks can set
// them. [New] calls ManagedHeaders once for each entry in
// [Config.HeaderOwners]. The names must obey the rules of
// [Config.ManagedHeaders], so a declaration cannot contain a hop-by-hop,
// framing, or Host header. New copies the names and does not retain the
// returned slice.
//
// Registration in [Config.Hooks] does not declare headers. If an owner is not
// in [Config.HeaderOwners], the gateway does not remove client values of its
// headers, unless another declaration contains them. An extension that
// implements HeaderOwner must tell its users to register it there.
type HeaderOwner interface {
	ManagedHeaders() []string
}

// compiledHooks holds independent phase lists in execution order (ADR-0030).
type compiledHooks struct {
	matched   []RouteMatchedHook
	before    []BeforeUpstreamHook
	responses []UpstreamResponseHook
	completed []RequestCompletedHook
}

// compileHooks copies the lists without calling hook methods.
func compileHooks(h Hooks) (compiledHooks, []error) {
	var problems []error
	problems = append(problems, checkHookList(h.RouteMatched, "route matched")...)
	problems = append(problems, checkHookList(h.BeforeUpstream, "before upstream")...)
	problems = append(problems, checkHookList(h.UpstreamResponse, "upstream response")...)
	problems = append(problems, checkHookList(h.RequestCompleted, "request completed")...)

	return compiledHooks{
		matched:   slices.Clone(h.RouteMatched),
		before:    slices.Clone(h.BeforeUpstream),
		responses: slices.Clone(h.UpstreamResponse),
		completed: slices.Clone(h.RequestCompleted),
	}, problems
}

// checkHookList reports every nil entry without calling hook methods.
func checkHookList[T any](list []T, phase string) []error {
	var problems []error
	for i, hook := range list {
		if isNilValue(hook) {
			problems = append(problems, invalid("hooks: %s %d: is nil", phase, i))
		}
	}

	return problems
}
