package gateway

import (
	"context"
	"net/url"
	"time"
)

// RequestContext holds gateway metadata and extension state for one request.
// The gateway owns its metadata. Extensions own values stored with StateKey.
// Access is not synchronized. Hooks run serially; extensions must not access
// this context concurrently or retain it after completion.
// Cancellation and deadlines remain on the standard context.Context.
type RequestContext struct {
	startedAt   time.Time
	routePrefix string
	upstream    *url.URL
	state       map[any]any
	err         error
}

type requestContextKey struct{}

// FromContext returns the gateway context attached before registered middleware.
// Middleware wrapping the gateway externally cannot access it before calling next.
func FromContext(ctx context.Context) (*RequestContext, bool) {
	rc, ok := ctx.Value(requestContextKey{}).(*RequestContext)
	return rc, ok
}

// StartedAt reports when the request entered the gateway.
func (rc *RequestContext) StartedAt() time.Time { return rc.startedAt }

// RoutePrefix reports the matched route, if routing reached a route handler.
func (rc *RequestContext) RoutePrefix() (string, bool) {
	return rc.routePrefix, rc.routePrefix != ""
}

// Upstream returns a copy of the selected destination, if proxying began.
func (rc *RequestContext) Upstream() (*url.URL, bool) {
	if rc.upstream == nil {
		return nil, false
	}
	upstream := *rc.upstream
	return &upstream, true
}

// StateKey identifies extension-owned state. Keep a key private to its owner
// and reuse it across requests. Distinct keys never share values.
// The nonzero size ensures that distinct key pointers have distinct identities.
type StateKey[T any] struct{ identity byte }

// NewStateKey creates a unique key for values of type T.
func NewStateKey[T any]() *StateKey[T] { return new(StateKey[T]) }

type stateValue[T any] struct{ value T }

// SetState stores a value for this request. A nil key panics.
func SetState[T any](rc *RequestContext, key *StateKey[T], value T) {
	if key == nil {
		panic("gateway: nil state key")
	}
	if rc.state == nil {
		rc.state = make(map[any]any)
	}
	rc.state[key] = stateValue[T]{value: value}
}

// GetState retrieves a value. A stored nil is distinct from a missing value.
// A nil key panics.
func GetState[T any](rc *RequestContext, key *StateKey[T]) (T, bool) {
	if key == nil {
		panic("gateway: nil state key")
	}
	entry, ok := rc.state[key].(stateValue[T])
	return entry.value, ok
}
