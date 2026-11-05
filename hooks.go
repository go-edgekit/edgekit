package edgekit

import (
	"context"
	"errors"
	"net/http"
	"slices"
)

// RequestReceivedHook is a lifecycle hook that runs after the global
// middleware and before route matching. Register it in [Config.Hooks].
//
// ctx is the context of the request that the global middleware passed on, and
// r is that request. The hook must not change r or read its body.
//
// The result of RequestReceived follows the rules of [Rejection]. After a
// rejection or an error, the later hooks of the phase do not run, and the
// gateway does not route the request. For an error, the gateway sends 500
// Internal Server Error without error details, or 499 when the error wraps
// [context.Canceled] and the client is gone, and completion gets the error as
// a [Failure] in [PhaseRequestReceived].
type RequestReceivedHook interface {
	RequestReceived(ctx context.Context, rc *RequestContext, r *http.Request) (*Rejection, error)
}

// BeforeUpstreamHook is a lifecycle hook that runs after the route middleware,
// the upstream selection, and the rewrite of the outbound request, before the
// transport sends it. Register it in [Config.Hooks]. Use it, for example, to
// sign the outbound request.
//
// out gives the rewritten outbound request. The hook can change only its
// header map, so the hook cannot change the destination. The gateway then
// applies the rules in the [Gateway] documentation to the header map. The hook
// runs once for each request, not once for each connection attempt of the
// transport. Register a provider that signs header fields after the providers
// that change those fields, so it is closer to the upstream (see
// [Config.Hooks]).
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

// UpstreamResponseHook is a lifecycle hook that runs after the upstream
// response headers arrive and before the gateway sends the final headers to
// the client. Register it in [Config.Hooks]. Use it, for example, to add
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

// ErrInvalidUpstreamResponse is the error that an [UpstreamResponseHook] wraps
// when it finds that the upstream response is not valid, such as a missing
// required signature. The gateway then sends 502 Bad Gateway, because the
// failure is in the upstream. In another phase, it has no effect on the
// status. Use [errors.Is] to identify it.
var ErrInvalidUpstreamResponse = errors.New("invalid upstream response")

// RequestCompletedHook is a lifecycle hook that runs once for each request,
// at completion: after the gateway-managed handler chain returned or finished
// unwinding, also during a panic, and after a rejection by the secure
// defaults. Register it in [Config.Hooks].
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

// HeaderOwner is an optional interface for a value in [Config.Hooks]. It
// declares the managed headers that an extension owns, such as a header that
// tells the upstream the authenticated caller.
//
// The gateway removes these headers from inbound requests together with the
// headers in [Config.ManagedHeaders], so only middleware and hooks can set
// them. [New] calls ManagedHeaders one time. The names must obey the rules of
// [Config.ManagedHeaders].
type HeaderOwner interface {
	ManagedHeaders() []string
}

// hooks holds the compiled hook lists in run order: the request phases in
// registration order, and the response phases in reverse order (ADR-0021).
type hooks struct {
	received  []RequestReceivedHook
	before    []BeforeUpstreamHook
	responses []UpstreamResponseHook
	completed []RequestCompletedHook
}

// compileHooks does not call hook methods, except ManagedHeaders.
func compileHooks(providers []any) (hooks, [][]string, []error) {
	var (
		compiled hooks
		problems []error
	)

	// declared has one entry for each provider, so the index is the location
	// in errors.
	declared := make([][]string, len(providers))

	for i, provider := range providers {
		if isNilValue(provider) {
			problems = append(problems, invalid("hooks %d: is nil", i))

			continue
		}

		found := false
		if hook, ok := provider.(RequestReceivedHook); ok {
			compiled.received = append(compiled.received, hook)
			found = true
		}

		if hook, ok := provider.(BeforeUpstreamHook); ok {
			compiled.before = append(compiled.before, hook)
			found = true
		}

		if hook, ok := provider.(UpstreamResponseHook); ok {
			compiled.responses = append(compiled.responses, hook)
			found = true
		}

		if hook, ok := provider.(RequestCompletedHook); ok {
			compiled.completed = append(compiled.completed, hook)
			found = true
		}

		if owner, ok := provider.(HeaderOwner); ok {
			declared[i] = owner.ManagedHeaders()
			found = true
		}

		if !found {
			problems = append(problems, invalid("hooks %d: %w", i, errNoHook))
		}
	}

	// The response phases go back from the upstream to the client, so the
	// innermost provider runs first.
	slices.Reverse(compiled.responses)
	slices.Reverse(compiled.completed)

	return compiled, declared, problems
}

// errNoHook describes a hook provider that implements no interface of a
// lifecycle hook or of HeaderOwner.
var errNoHook = errors.New("implements no hook interface and not HeaderOwner")
