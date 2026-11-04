package edgekit

import (
	"fmt"
	"net/http"
	"slices"
)

// Middleware wraps HTTP handling. It can examine or change the request, write
// a response, or end handling early when it does not call next.
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
// check (see [Gateway]). Otherwise the gateway sends 500 Internal Server
// Error and does not call the upstream.
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
