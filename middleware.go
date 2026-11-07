package edgekit

import (
	"fmt"
	"net/http"
	"slices"
)

// Middleware wraps request processing after route selection and protection.
// It can examine or change the request, write a response, or end handling
// early when it does not call next.
//
// Middleware uses the standard net/http model, so existing middleware packages
// work without changes. A middleware must call next synchronously and at most
// once. A middleware that calls next must not write a final status or a body,
// or flush or hijack the response, before it calls next, because the gateway
// can then not send a rejection or a failure. Setting response headers and
// sending informational (1xx) responses before next is permitted. Middleware
// that returns while the next handler continues, such as [http.TimeoutHandler],
// is not supported.
//
// A middleware must not change the request that it gets, as [http.Handler]
// also requires. The request can be the request that the caller gave to
// [Gateway.ServeHTTP]. To change the request, make a copy with
// [http.Request.Clone], change the copy, and give the copy to next. The
// headers and trailer names that middleware sets must obey the outbound header
// check (see [Gateway]). Changes to path/query, method, and URL fields must
// also obey the applicable request protections. Invalid output gives 500
// without forwarding. Routing and its protections remain fixed: a path
// change is an outbound rewrite, not a request to select another route.
//
// Middleware that replaces or expands the body owns its output bounds, body
// closure, ContentLength, trailers, and GetBody. The inbound limit does not
// bound generated content. The gateway clears a caller-provided inbound
// GetBody; only explicit buffering supplies gateway-owned replay. For
// buffered requests, replay is request-scoped: close a consumed cursor and
// explicitly replace Body with a GetBody cursor to forward the complete
// buffered content. Never retain these capabilities after handling. See
// [RequestBuffering].
//
// The gateway calls each middleware function one time, in [New], and uses the
// returned handler for all requests. The returned handler must be safe for
// concurrent use. It must not be nil.
type Middleware func(next http.Handler) http.Handler

// chain wraps handler so that the first middleware is the outermost one.
func chain(handler http.Handler, middleware []Middleware) (http.Handler, error) {
	for i, m := range slices.Backward(middleware) {
		handler = m(handler)
		if handler == nil {
			return nil, fmt.Errorf("middleware %d: returned a nil handler", i)
		}
	}

	return handler, nil
}

// isNil exists for slices.IndexFunc, because function values are not
// comparable.
func isNil(m Middleware) bool {
	return m == nil
}
