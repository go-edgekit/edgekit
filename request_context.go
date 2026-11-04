package edgekit

import (
	"context"
	"net/url"
	"time"
)

// RequestContext is the gateway context of one request. The gateway makes it
// before the global middleware runs, and its lifetime ends with request
// completion. Get it with [FromContext] in middleware, and as an argument in
// lifecycle hooks.
//
// It gives a read-only view of gateway metadata: the start time, the matched
// route, and the selected upstream. Extensions keep their request state on it
// with [SetState] and [State], not on shared extension values.
//
// Contract:
//   - Access is not synchronized. The gateway runs middleware and hooks of one
//     request serially. Do not use a RequestContext from more than one
//     goroutine at the same time.
//   - Do not keep a RequestContext or its state after completion. After the
//     completion hooks return, the gateway drops the state: [State] reports
//     no value and [SetState] has no effect.
//   - Only the gateway makes a RequestContext. The zero value is not useful.
type RequestContext struct {
	start     time.Time
	route     string
	upstream  *url.URL
	rejection RejectionReason
	failure   *Failure
	state     map[any]any
	ended     bool

	// outbound is reused for the BeforeUpstream hooks, so the view needs no
	// allocation of its own.
	outbound OutboundRequest
}

// requestContextKey is the context key for the gateway context.
type requestContextKey struct{}

// FromContext returns the gateway context that ctx carries. It reports false
// when ctx does not come from a request that the gateway handles, such as in
// middleware that wraps the gateway from outside.
//
// Middleware must derive the context of the request that it passes on from
// the incoming request context, so inner stages can find the gateway context.
func FromContext(ctx context.Context) (*RequestContext, bool) {
	rc, ok := ctx.Value(requestContextKey{}).(*RequestContext)

	return rc, ok
}

// StartTime returns the time when the request entered the gateway.
func (rc *RequestContext) StartTime() time.Time {
	return rc.start
}

// RoutePrefix returns the prefix of the matched route. It reports false before
// routing and when no route matched.
func (rc *RequestContext) RoutePrefix() (string, bool) {
	return rc.route, rc.route != ""
}

// SelectedUpstream returns a copy of the URL that the upstream selector
// returned. It reports false before the selection and when the selection
// failed. A selected upstream does not prove that the gateway connected to it.
func (rc *RequestContext) SelectedUpstream() (*url.URL, bool) {
	if rc.upstream == nil {
		return nil, false
	}

	upstream := *rc.upstream

	return &upstream, true
}

// fail keeps the first failure, because later failures are consequences of it.
func (rc *RequestContext) fail(phase Phase, err error) {
	if rc.failure == nil {
		rc.failure = &Failure{
			Phase: phase,
			Err:   err,
		}
	}
}

// end drops the state, so a kept RequestContext does not keep values alive.
func (rc *RequestContext) end() {
	rc.state = nil
	rc.ended = true
}

// StateKey identifies request state of type T. Each key has its own identity,
// so two keys never share a value, also for the same T.
//
// Make a key once with [NewStateKey], keep it in a package variable, and use
// it for all requests. Keep the key unexported to give its owner control of
// the state. To share state with other extensions, export the key or
// functions that use it, and document the contract.
type StateKey[T any] struct {
	// The field gives the type a nonzero size, so two keys never have the
	// same address.
	_ byte
}

// NewStateKey returns a new key for request state of type T.
func NewStateKey[T any]() *StateKey[T] {
	return &StateKey[T]{}
}

// SetState stores value for key in the request state of rc. It replaces an
// earlier value for key. A nil value is a value: [State] then reports true.
//
// SetState panics if rc or key is nil. It has no effect after completion. Do
// not call it concurrently with other access to rc (see [RequestContext]).
func SetState[T any](rc *RequestContext, key *StateKey[T], value T) {
	if key == nil {
		panic("edgekit: SetState with a nil state key")
	}

	if rc.ended {
		return
	}

	if rc.state == nil {
		rc.state = make(map[any]any)
	}

	rc.state[key] = stateValue[T]{
		value: value,
	}
}

// stateValue keeps the type T also for a nil interface value, which has no
// dynamic type of its own.
type stateValue[T any] struct {
	value T
}

// State returns the value for key in the request state of rc. It reports false
// when no value is stored for key, and after completion.
//
// State panics if rc or key is nil. Do not call it concurrently with
// [SetState] for the same rc.
func State[T any](rc *RequestContext, key *StateKey[T]) (T, bool) {
	if key == nil {
		panic("edgekit: State with a nil state key")
	}

	stored, ok := rc.state[key].(stateValue[T])

	return stored.value, ok
}
